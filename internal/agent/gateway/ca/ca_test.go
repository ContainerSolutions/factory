package ca

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/clock"
)

var t0 = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func generate(t *testing.T, clk clock.Clock) (*CA, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gateway-ca")
	c, err := Generate(context.Background(), dir, clk)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return c, dir
}

func verify(c *CA, der []byte, name string, at time.Time) error {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: c.Pool(), DNSName: name, CurrentTime: at,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestGenerateFiles(t *testing.T) {
	_, dir := generate(t, clock.NewFake(t0))
	for name, want := range map[string]os.FileMode{".": 0o700, CertFile: 0o644, KeyFile: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %#o", name, fi.Mode(), want)
		}
	}
	if _, err := Generate(context.Background(), dir, clock.NewFake(t0)); !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second Generate: %v, want fs.ErrExist", err)
	}

	// A certificate that can't be written takes its new key with it.
	stuck := filepath.Join(t.TempDir(), "gateway-ca")
	if err := os.MkdirAll(filepath.Join(stuck, CertFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), stuck, clock.NewFake(t0)); err == nil {
		t.Fatal("Generate over a directory named ca.pem succeeded")
	}
	if _, err := os.Stat(filepath.Join(stuck, KeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the key was left behind: %v", err)
	}
}

// TestNameConstraints: a leaf verifies for every intercepted host, and nothing else the CA
// could sign does: another DNS name, an IP, URI or email SAN.
func TestNameConstraints(t *testing.T) {
	c, _ := generate(t, clock.NewFake(t0))
	for _, h := range policy.InterceptedHosts() {
		l, err := c.Leaf(h)
		if err != nil {
			t.Fatalf("Leaf(%s): %v", h, err)
		}
		if err := verify(c, l.Certificate[0], h, t0); err != nil {
			t.Errorf("the leaf for %s: %v", h, err)
		}
	}
	u, _ := url.Parse("https://evil.example.com/")
	tests := []struct {
		name string
		tmpl x509.Certificate
		dns  string
	}{
		{"another name", x509.Certificate{DNSNames: []string{"evil.example.com"}}, "evil.example.com"},
		{"a suffix trick", x509.Certificate{DNSNames: []string{"evilgithub.com"}}, "evilgithub.com"},
		{"an IP SAN", x509.Certificate{DNSNames: []string{"github.com"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}, "10.0.0.1"},
		{"an IPv6 SAN", x509.Certificate{DNSNames: []string{"github.com"}, IPAddresses: []net.IP{net.ParseIP("::1")}}, "::1"},
		{"a URI SAN", x509.Certificate{DNSNames: []string{"github.com"}, URIs: []*url.URL{u}}, "github.com"},
		{"an email SAN", x509.Certificate{DNSNames: []string{"github.com"}, EmailAddresses: []string{"a@evil.example.com"}}, "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := tt.tmpl
			tmpl.Subject = pkix.Name{CommonName: "x"}
			tmpl.NotBefore, tmpl.NotAfter = t0.Add(-time.Hour), t0.Add(time.Hour)
			l, err := c.sign(&tmpl)
			if err != nil {
				t.Fatal(err)
			}
			var ce x509.CertificateInvalidError
			if err := verify(c, l.Certificate[0], tt.dns, t0); !errors.As(err, &ce) || ce.Reason != x509.CANotAuthorizedForThisName {
				t.Errorf("verify: %v, want CANotAuthorizedForThisName", err)
			}
		})
	}
	if _, err := c.Leaf("example.com"); err == nil {
		t.Error("Leaf minted a leaf for a host that isn't intercepted")
	}
}

func TestLoad(t *testing.T) {
	clk := clock.NewFake(t0)
	orig, dir := generate(t, clk)
	got, err := LoadOrGenerate(context.Background(), dir, clk)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !got.Certificate().Equal(orig.Certificate()) || string(got.CertPEM()) != string(orig.CertPEM()) {
		t.Error("LoadOrGenerate made a new CA instead of loading the existing one")
	}
	fresh := filepath.Join(t.TempDir(), "new")
	if _, err := LoadOrGenerate(context.Background(), fresh, clk); err != nil {
		t.Fatalf("LoadOrGenerate on a missing directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, KeyFile)); err != nil {
		t.Errorf("no key generated: %v", err)
	}

	_, otherDir := generate(t, clk)
	otherKey, _ := os.ReadFile(filepath.Join(otherDir, KeyFile))
	tests := []struct {
		name string
		mut  func(t *testing.T, dir string)
		at   time.Time
	}{
		{"a key readable by others", func(t *testing.T, d string) { chmod(t, filepath.Join(d, KeyFile), 0o644) }, t0},
		{"a key that isn't a regular file", func(t *testing.T, d string) {
			k := filepath.Join(d, KeyFile)
			_ = os.Remove(k)
			if err := os.Mkdir(k, 0o700); err != nil {
				t.Fatal(err)
			}
		}, t0},
		{"another CA's key", func(t *testing.T, d string) {
			if err := os.WriteFile(filepath.Join(d, KeyFile), otherKey, 0o600); err != nil {
				t.Fatal(err)
			}
		}, t0},
		{"not PEM", func(t *testing.T, d string) {
			if err := os.WriteFile(filepath.Join(d, CertFile), []byte("junk"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, t0},
		{"expired", func(*testing.T, string) {}, t0.Add(caValidity)},
		{"not yet valid", func(*testing.T, string) {}, t0.Add(-2 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := copyDir(t, dir)
			tt.mut(t, d)
			if _, err := Load(context.Background(), d, clock.NewFake(tt.at)); !errors.Is(err, ErrInvalid) {
				t.Errorf("Load: %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLeafCache(t *testing.T) {
	clk := clock.NewFake(t0)
	c, _ := generate(t, clk)
	a, err := c.Leaf("GitHub.com.")
	if err != nil {
		t.Fatal(err)
	}
	if a.Leaf.NotAfter != t0.Add(leafValidity) || a.Leaf.NotBefore != t0.Add(-leafBackdate) {
		t.Errorf("validity %v to %v", a.Leaf.NotBefore, a.Leaf.NotAfter)
	}
	if b, _ := c.Leaf("github.com"); b != a {
		t.Error("a second Leaf minted again")
	}
	clk.Advance(leafValidity - leafRemint + time.Second)
	b, _ := c.Leaf("github.com")
	if b == a {
		t.Fatal("a leaf with less than an hour left was reused")
	}
	if err := verify(c, b.Certificate[0], "github.com", clk.Now()); err != nil {
		t.Errorf("the re-minted leaf: %v", err)
	}
	clk.Advance(caValidity) // past the CA's own NotAfter: the leaf is capped at it
	if l, _ := c.Leaf("claude.ai"); !l.Leaf.NotAfter.Equal(c.Certificate().NotAfter) {
		t.Errorf("NotAfter %v, want the CA's %v", l.Leaf.NotAfter, c.Certificate().NotAfter)
	}
}

func TestLeafCacheBound(t *testing.T) {
	c, _ := generate(t, clock.NewFake(t0))
	// Only the intercepted hosts reach Leaf, fewer than the bound: fill the cache to it directly.
	for i := range cacheSize {
		h := fmt.Sprintf("fill-%d", i)
		c.leaves[h] = c.lru.PushFront(&leaf{host: h, notAfter: t0.Add(leafValidity)})
	}
	if _, err := c.Leaf("chatgpt.com"); err != nil {
		t.Fatal(err)
	}
	if n, m := c.lru.Len(), len(c.leaves); n != cacheSize || m != cacheSize {
		t.Errorf("cache holds %d (map %d), want %d", n, m, cacheSize)
	}
	_, newest := c.leaves["chatgpt.com"]
	_, oldest := c.leaves["fill-0"]
	if !newest || oldest {
		t.Errorf("newest kept %v, oldest kept %v: want the least recently used evicted", newest, oldest)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "gateway-ca")
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	chmod(t, filepath.Join(dst, KeyFile), 0o600) // CopyFS doesn't keep modes
	return dst
}
