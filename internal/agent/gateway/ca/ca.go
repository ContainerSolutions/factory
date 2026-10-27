// Package ca is the agent host's gateway CA (gateway.md, "The CA"): a name-constrained ECDSA
// P-256 certificate authority whose leaves the gateway presents for the hosts it intercepts.
//
// The CA lives in a directory of its own (0700): CertFile (0644) and KeyFile (0600, PKCS #8).
// Its name constraints permit only the intercepted hosts' DNS names, exclude every IP address,
// and permit URI and email names only under "invalid", so a leaked key can't mint a certificate
// a client would accept for any other site. Leaves are minted on first use and kept in a
// bounded in-memory cache; they never touch the disk.
//
// The PoC has no rotation: LoadOrGenerate makes a CA when there is none, and an expired or
// out-of-date one is replaced by removing its directory.
package ca

import (
	"container/list"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// The CA directory's files.
const (
	CertFile = "ca.pem"
	KeyFile  = "ca.key"
)

const (
	commonName = "factory-agent gateway CA"
	caValidity = 365 * 24 * time.Hour
	// leafValidity is a leaf's life from its mint; it starts leafBackdate earlier, for clock skew.
	leafValidity = 7 * 24 * time.Hour
	leafBackdate = time.Hour
	// leafRemint is how close to its NotAfter a cached leaf is minted again.
	leafRemint = time.Hour
	// cacheSize bounds the leaf cache; the intercepted host list bounds it anyway.
	cacheSize = 64
	maxFile   = 64 << 10
)

// ErrInvalid is wrapped by every refusal of Load: a file of the wrong kind, mode or owner, a
// certificate that isn't a CA or isn't valid now, or a key that doesn't match it.
var ErrInvalid = errors.New("ca: invalid gateway CA")

// CA signs the gateway's leaves. It is safe for concurrent use.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	clk  clock.Clock

	mu     sync.Mutex
	leaves map[string]*list.Element // of *leaf, most recently used first
	lru    list.List
}

type leaf struct {
	host     string
	cert     *tls.Certificate
	notAfter time.Time
}

// Generate makes a new CA in dir (created 0700 when missing) and returns it. It refuses to
// replace an existing CertFile or KeyFile. clk times the CA's and its leaves' validity.
func Generate(ctx context.Context, dir string, clk clock.Clock) (*CA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generate the key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := clk.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-leafBackdate),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// RFC 5280 constrains each name type on its own: without the IP exclusions and the
		// "invalid" URI and email subtrees, the CA could still sign for any address or URI.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         policy.InterceptedHosts(),
		ExcludedIPRanges:            []*net.IPNet{{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, {IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}},
		PermittedURIDomains:         []string{"invalid"},
		PermittedEmailAddresses:     []string{"invalid"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca: create the certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("ca: encode the key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ca: create %s: %w", dir, err)
	}
	// The key first, removed again when the certificate can't be written: a CertFile without
	// its key never exists, and a failed Generate leaves no key for the next one to trip on.
	keyPath := filepath.Join(dir, KeyFile)
	if err := writeNew(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeNew(filepath.Join(dir, CertFile), certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)
		return nil, err
	}
	return newCA(der, key, certPEM, clk)
}

// writeNew writes b to a file that must not exist yet, with mode perm.
func writeNew(path string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("ca: create %s: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	return nil
}

// Load reads the CA in dir. Both files are read through one handle on the directory. It
// refuses (ErrInvalid) a KeyFile that isn't a regular file of this user's with mode 0600, a
// certificate that isn't a CA or isn't valid on clk, and a key that doesn't match it. A missing
// directory or file is an fs.ErrNotExist error.
func Load(ctx context.Context, dir string, clk clock.Clock) (*CA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("ca: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	keyPEM, err := readFile(root, KeyFile, true)
	if err != nil {
		return nil, err
	}
	certPEM, err := readFile(root, CertFile, false)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%w: %s is not a PEM private key", ErrInvalid, KeyFile)
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, KeyFile, err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an ECDSA key", ErrInvalid, KeyFile)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: %s is not a PEM certificate", ErrInvalid, CertFile)
	}
	return newCA(cb.Bytes, key, pem.EncodeToMemory(cb), clk)
}

// LoadOrGenerate loads the CA in dir, or generates one when the directory or the certificate
// doesn't exist.
func LoadOrGenerate(ctx context.Context, dir string, clk clock.Clock) (*CA, error) {
	c, err := Load(ctx, dir, clk)
	if errors.Is(err, fs.ErrNotExist) {
		return Generate(ctx, dir, clk)
	}
	return c, err
}

// readFile reads name in root: a regular file of at most maxFile bytes and, for the key, this
// user's with mode 0600.
func readFile(root *os.Root, name string, secret bool) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("ca: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ca: stat %s: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalid, name)
	}
	if secret {
		if fi.Mode().Perm() != 0o600 {
			return nil, fmt.Errorf("%w: %s has mode %#o, want 0600", ErrInvalid, name, fi.Mode().Perm())
		}
		if !ownedByMe(fi) {
			return nil, fmt.Errorf("%w: %s is not this user's", ErrInvalid, name)
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("ca: read %s: %w", name, err)
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("%w: %s is too large", ErrInvalid, name)
	}
	return b, nil
}

// newCA checks the certificate against its key and clk.
func newCA(der []byte, key *ecdsa.PrivateKey, certPEM []byte, clk clock.Clock) (*CA, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, CertFile, err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, fmt.Errorf("%w: %s is not a CA", ErrInvalid, CertFile)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("%w: %s does not match %s", ErrInvalid, KeyFile, CertFile)
	}
	if now := clk.Now(); now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, fmt.Errorf("%w: %s is not valid now (%s to %s)", ErrInvalid, CertFile,
			cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
	}
	return &CA{cert: cert, key: key, pem: certPEM, clk: clk, leaves: map[string]*list.Element{}}, nil
}

// CertPEM is the CA certificate in PEM: the public half a workspace's bundle carries.
func (c *CA) CertPEM() []byte { return slices.Clone(c.pem) }

// Certificate is the parsed CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// Pool is a certificate pool holding only the CA.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Leaf is the leaf certificate for host, one of policy.InterceptedHosts (normalised): minted on
// first use, valid from an hour before for 7 days (capped at the CA's NotAfter), and minted
// again when less than an hour is left. Its chain holds the CA.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = policy.NormalizeHost(host)
	if !slices.Contains(policy.InterceptedHosts(), host) {
		return nil, fmt.Errorf("ca: %q is not an intercepted host", host)
	}
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.leaves[host]; ok {
		l := e.Value.(*leaf)
		if l.notAfter.Sub(now) >= leafRemint {
			c.lru.MoveToFront(e)
			return l.cert, nil
		}
		c.lru.Remove(e)
		delete(c.leaves, host)
	}
	notAfter := now.Add(leafValidity)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	cert, err := c.sign(&x509.Certificate{
		Subject:   pkix.Name{CommonName: host},
		DNSNames:  []string{host},
		NotBefore: now.Add(-leafBackdate),
		NotAfter:  notAfter,
	})
	if err != nil {
		return nil, err
	}
	c.leaves[host] = c.lru.PushFront(&leaf{host: host, cert: cert, notAfter: notAfter})
	for c.lru.Len() > cacheSize {
		old := c.lru.Back()
		c.lru.Remove(old)
		delete(c.leaves, old.Value.(*leaf).host)
	}
	return cert, nil
}

// sign mints a server leaf from tmpl's names and validity, with a new P-256 key.
func (c *CA) sign(tmpl *x509.Certificate) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generate a leaf key: %w", err)
	}
	if tmpl.SerialNumber, err = newSerial(); err != nil {
		return nil, err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("ca: sign a leaf: %w", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: parse a leaf: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: parsed}, nil
}

func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca: serial number: %w", err)
	}
	return n, nil
}
