// Package vmrun is factory-agent run's runtime inside a VM workspace (workspace-run.md). This
// file is its system setup: the environment that points every tool at the gateway (the
// proxies, the CA bundle, the placeholders) and, with --system-setup on, the privileged part
// (the system trust store, dockerd's proxy).
package vmrun

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/clock"
)

// SystemSetup is --system-setup: whether Setup runs its privileged part. There is no
// automatic mode: the supervisor, which knows it launched a workspace VM, passes SetupOn, and
// anything else gets the default, SetupOff, so a workstation or a CI runner with cached or
// passwordless sudo never has its trust store or dockerd changed by a guess.
type SystemSetup string

const (
	// SetupOn installs the gateway CA into the system store and gives dockerd the proxy.
	SetupOn SystemSetup = "on"
	// SetupOff only builds the environment and the files under the state dir.
	SetupOff SystemSetup = "off"
	// DefaultSystemSetup is --system-setup's default.
	DefaultSystemSetup = SetupOff
)

// ParseSystemSetup parses --system-setup: on or off.
func ParseSystemSetup(s string) (SystemSetup, error) {
	switch m := SystemSetup(s); m {
	case SetupOn, SetupOff:
		return m, nil
	case "auto":
		return "", errors.New(`--system-setup "auto": there is no automatic mode; pass on in a workspace VM (the supervisor does), else off (the default)`)
	}
	return "", fmt.Errorf("--system-setup %q: want on or off", s)
}

// ParseProxy parses --proxy: an absolute http URL with a host and a port and nothing else
// (no user info, path, query or fragment).
func ParseProxy(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("--proxy: %w", err)
	}
	if err := checkProxy(u); err != nil {
		return nil, err
	}
	return u, nil
}

func checkProxy(u *url.URL) error {
	switch {
	case u == nil:
		return errors.New("--proxy is required")
	case u.Scheme != "http":
		return fmt.Errorf("--proxy %q: the scheme must be http", u.Redacted())
	case u.User != nil:
		return errors.New("--proxy: user info isn't allowed")
	case u.Hostname() == "" || u.Port() == "":
		return fmt.Errorf("--proxy %q: want http://<host>:<port>", u.Redacted())
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return fmt.Errorf("--proxy %q: only a host and a port are allowed", u.Redacted())
	}
	return nil
}

// DefaultNoProxy is what NO_PROXY always holds: the VM's own and the private addresses (the
// environment's services, a kind cluster's), before the manifest's Gateway.NoProxy.
var DefaultNoProxy = []string{
	"localhost", "127.0.0.1", "::1", ".localhost",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", ".svc", ".cluster.local",
}

// The system paths Setup reads and writes, under System.Root.
const (
	systemRoots = "/etc/ssl/certs/ca-certificates.crt"
	caInstall   = "/usr/local/share/ca-certificates/factory-gateway.crt"
	daemonJSON  = "/etc/docker/daemon.json"
)

// PlaywrightBrowsersPath is where the base VM installs Playwright's Chromium
// (provision/base.sh's PLAYWRIGHT_BROWSERS). Setup exports it as PLAYWRIGHT_BROWSERS_PATH:
// Playwright's default, ~/.cache/ms-playwright, is under the pipeline's scratch HOME.
const PlaywrightBrowsersPath = "/opt/ms-playwright"

// tmpSuffix names the temp file beside a system file that SetupOn installs and then renames
// over it: /etc/docker/.daemon.json.factory-tmp.
const tmpSuffix = ".factory-tmp"

// The files Setup writes under the state dir.
const (
	CABundleFile = "ca-bundle.pem"
	DockerDir    = "docker"
	stageDir     = "system" // the files SetupOn installs, staged; removed after
)

const (
	// DefaultDockerTimeout bounds the wait for dockerd after its restart.
	DefaultDockerTimeout = 60 * time.Second
	dockerPoll           = time.Second
	dockerAttempt        = 10 * time.Second // one docker info
	maxSystemRoots       = 8 << 20
	maxDaemonJSON        = 1 << 20
	maxOutputTail        = 2 << 10 // a failed command's output kept for its error
)

// Runner runs a command, with its stdout and stderr (which may be the same writer).
type Runner interface {
	Run(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}

// ExecRunner runs real programs, found on PATH.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// System is what Setup runs against. Tests replace the commands, the clock, the user and the
// file system root, so nothing privileged runs outside a VM.
type System struct {
	// StateDir holds what the runtime writes outside the workspace (--state-dir): the CA
	// bundle and the docker client config. Required, absolute.
	StateDir string
	// WorkDir is the workspace root (--work, pipeline.Options.Root); "" skips it. Setup makes
	// it when it's absent, with sudo when its parent isn't writable by the run's user.
	WorkDir string
	// Root prefixes the system paths (/etc, /usr/local); "" is /.
	Root string
	// Runner runs sudo, install, cat, mv, update-ca-certificates, systemctl and docker;
	// default ExecRunner.
	Runner Runner
	// Clock times the wait for dockerd; default clock.Real.
	Clock clock.Clock
	// User is the run's user (its uid, gid and home); default user.Current.
	User func() (*user.User, error)
	// DockerTimeout bounds the wait for docker info; default DefaultDockerTimeout.
	DockerTimeout time.Duration
	// Logger gets the setup's progress; default slog.Default.
	Logger *slog.Logger
}

// setup is one Setup call.
type setup struct {
	System
	root  bool // the run is root: no sudo
	user  *user.User
	proxy string
	noPx  string
}

// Setup prepares the VM for the run (workspace-run.md §System setup) and returns the
// environment that becomes pipeline.Options.Env and the environment runner's Env. The CA
// bundle and the docker client config are written in every mode; SetupOn also installs the
// gateway CA into the system store, gives dockerd the proxy and restarts it. Every error fails
// the run environment-error.
func (s System) Setup(ctx context.Context, b *runbundle.Bundle, proxy *url.URL, mode SystemSetup) (map[string]string, error) {
	if err := s.defaults(); err != nil {
		return nil, err
	}
	if err := checkProxy(proxy); err != nil {
		return nil, fmt.Errorf("system setup: %w", err)
	}
	if b == nil || b.CAFile == "" {
		return nil, errors.New("system setup: the bundle has no gateway CA")
	}
	raw, err := readCapped(b.CAFile, runbundle.MaxCABytes)
	if err != nil {
		return nil, fmt.Errorf("system setup: the gateway CA: %w", err)
	}
	ca, err := parseCertificates(raw)
	if err != nil {
		return nil, fmt.Errorf("system setup: the gateway CA: %w", err)
	}
	u, err := s.User()
	if err != nil {
		return nil, fmt.Errorf("system setup: the run's user: %w", err)
	}
	if mode != SetupOn && mode != SetupOff {
		return nil, fmt.Errorf("system setup: unknown mode %q", mode)
	}
	r := &setup{System: s, root: u.Uid == "0", user: u, proxy: proxy.String(), noPx: noProxy(b.Manifest.Gateway.NoProxy)}
	r.Logger.Info("system setup", "mode", mode)
	if err := r.dirs(ctx, mode); err != nil {
		return nil, err
	}
	bundle := filepath.Join(s.StateDir, CABundleFile)
	if err := r.writeCABundle(bundle, ca); err != nil {
		return nil, err
	}
	docker := filepath.Join(s.StateDir, DockerDir)
	if err := writeDockerConfig(docker); err != nil {
		return nil, err
	}
	if mode == SetupOn {
		if err := r.privileged(ctx, ca); err != nil {
			return nil, err
		}
	}
	env := map[string]string{
		"HTTPS_PROXY": r.proxy, "HTTP_PROXY": r.proxy, "https_proxy": r.proxy, "http_proxy": r.proxy,
		"NO_PROXY": r.noPx, "no_proxy": r.noPx,
		"SSL_CERT_FILE": bundle, "GIT_SSL_CAINFO": bundle, "CURL_CA_BUNDLE": bundle, "REQUESTS_CA_BUNDLE": bundle,
		"NODE_EXTRA_CA_CERTS": b.CAFile,
		"GH_TOKEN":            harness.Placeholder,
		pipeline.EnvRuntime:   pipeline.RuntimeWorkspace,
		"DOCKER_CONFIG":       docker,
		// docs-gif's tools in the base (provision/base.sh): Playwright's browsers, and vhs's
		// Chromium without its sandbox, which Ubuntu 24.04's AppArmor keeps from user
		// namespaces (Playwright turns it off itself). The VM is the task's sandbox.
		"PLAYWRIGHT_BROWSERS_PATH": PlaywrightBrowsersPath,
		"VHS_NO_SANDBOX":           "1",
		// Node's own fetch and http clients ignore the proxy variables unless this is set, and
		// the VM reaches nothing but the gateway: corepack's yarn and pnpm shims (base.sh's
		// corepack enable) download the package manager with fetch on first use. Node's
		// NO_PROXY has no CIDR, so only its host-name and single-address entries stay direct.
		"NODE_USE_ENV_PROXY": "1",
		// corepack's download prompt would wait on a terminal's stdin.
		"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
	}
	// The pipeline's scratch HOME would hide the gh extensions the base installed for the
	// run's user under its own ~/.local/share.
	if u.HomeDir != "" {
		env["XDG_DATA_HOME"] = filepath.Join(u.HomeDir, ".local", "share")
	} else {
		r.Logger.Warn("system setup: the run's user has no home directory; XDG_DATA_HOME is unset")
	}
	return env, nil
}

func (s *System) defaults() error {
	if s.StateDir == "" || !filepath.IsAbs(s.StateDir) {
		return fmt.Errorf("system setup: the state dir %q must be absolute", s.StateDir)
	}
	if s.WorkDir != "" && !filepath.IsAbs(s.WorkDir) {
		return fmt.Errorf("system setup: the work dir %q must be absolute", s.WorkDir)
	}
	if s.Root == "" {
		s.Root = "/"
	}
	if s.Runner == nil {
		s.Runner = ExecRunner{}
	}
	if s.Clock == nil {
		s.Clock = clock.Real{}
	}
	if s.User == nil {
		s.User = user.Current
	}
	if s.DockerTimeout <= 0 {
		s.DockerTimeout = DefaultDockerTimeout
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	return nil
}

// noProxy is DefaultNoProxy followed by the manifest's entries, each once.
func noProxy(extra []string) string {
	l := slices.Clone(DefaultNoProxy)
	for _, h := range extra {
		if !slices.Contains(l, h) {
			l = append(l, h)
		}
	}
	return strings.Join(l, ",")
}

// dirs makes the state dir and the work dir when they're absent, with their missing parents:
// 0700, by the run's user when it can, else, with SetupOn, by sudo -n install -d owned by the
// run's user (the default state dir's /var/lib/factory-agent isn't in the base). An existing
// directory is left as it is (the pipeline checks that the work dir is empty); a symlink or a
// file is refused.
func (r *setup) dirs(ctx context.Context, mode SystemSetup) error {
	if err := r.ensureDir(ctx, r.StateDir, mode); err != nil {
		return fmt.Errorf("system setup: the state dir: %w", err)
	}
	if r.WorkDir != "" {
		if err := r.ensureDir(ctx, r.WorkDir, mode); err != nil {
			return fmt.Errorf("system setup: the work dir: %w", err)
		}
	}
	return nil
}

func (r *setup) ensureDir(ctx context.Context, dir string, mode SystemSetup) error {
	fi, err := os.Lstat(dir)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s exists and is not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	err = os.MkdirAll(dir, 0o700)
	if err == nil || !errors.Is(err, fs.ErrPermission) || mode != SetupOn || r.root {
		return err
	}
	return r.priv(ctx, "install", "-d", "-o", r.user.Uid, "-g", r.user.Gid, "-m", "0700", dir)
}

// writeCABundle writes the system roots followed by the gateway CA. A system without roots
// (a minimal container) gets the gateway CA alone, with a warning.
func (r *setup) writeCABundle(dst string, ca []byte) error {
	roots, err := readCapped(r.path(systemRoots), maxSystemRoots)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Logger.Warn("system setup: no system CA roots; the CA bundle holds only the gateway CA", "path", systemRoots)
	case err != nil:
		return fmt.Errorf("system setup: the system CA roots: %w", err)
	}
	var buf bytes.Buffer
	buf.Write(roots)
	if len(roots) > 0 && !bytes.HasSuffix(roots, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.Write(ca)
	if err := writeFileAtomic(dst, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("system setup: the CA bundle: %w", err)
	}
	return nil
}

// writeDockerConfig writes the docker client's config.json with no proxies key (decided
// 2026-10-22, risk 13): containers get no proxy variables, so calls between compose services
// by name stay direct; image pulls go through dockerd's own proxy.
func writeDockerConfig(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("system setup: the docker config: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "config.json"), []byte("{}\n"), 0o600); err != nil {
		return fmt.Errorf("system setup: the docker config: %w", err)
	}
	return nil
}

// privileged is SetupOn's part, each command with sudo -n (none as root): the gateway CA into
// the system store, dockerd's proxy, its restart and the wait for it.
func (r *setup) privileged(ctx context.Context, ca []byte) error {
	stage := filepath.Join(r.StateDir, stageDir)
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("system setup: %w", err)
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return fmt.Errorf("system setup: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	r.Logger.Info("system setup: installing the gateway CA")
	if err := r.install(ctx, stage, caInstall, ca); err != nil {
		return fmt.Errorf("system setup: the gateway CA: %w", err)
	}
	if err := r.priv(ctx, "update-ca-certificates"); err != nil {
		return fmt.Errorf("system setup: %w", err)
	}

	r.Logger.Info("system setup: giving dockerd the proxy")
	daemon, err := r.daemonConfig(ctx)
	if err != nil {
		return fmt.Errorf("system setup: %s: %w", daemonJSON, err)
	}
	if err := r.install(ctx, stage, daemonJSON, daemon); err != nil {
		return fmt.Errorf("system setup: %s: %w", daemonJSON, err)
	}
	if err := r.priv(ctx, "systemctl", "restart", "docker"); err != nil {
		return fmt.Errorf("system setup: %w", err)
	}
	if err := r.waitDocker(ctx); err != nil {
		return fmt.Errorf("system setup: %w", err)
	}
	return nil
}

// install stages content in the state dir and puts it at the system path p atomically: it
// installs it as a temp file in p's directory (its directory made 0755 when absent), with the
// mode of the file it replaces, else 0644, then renames that over p with mv -f. A run killed
// midway leaves p as it was, never missing or truncated.
func (r *setup) install(ctx context.Context, stage, p string, content []byte) error {
	dst := r.path(p)
	mode := fs.FileMode(0o644)
	switch fi, err := os.Stat(dst); {
	case err == nil:
		mode = fi.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	src := filepath.Join(stage, filepath.Base(p))
	if err := os.WriteFile(src, content, 0o600); err != nil {
		return err
	}
	if err := r.priv(ctx, "install", "-d", "-m", "0755", filepath.Dir(dst)); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+tmpSuffix)
	if err := r.priv(ctx, "install", "-m", fmt.Sprintf("%04o", mode), src, tmp); err != nil {
		return err
	}
	return r.priv(ctx, "mv", "-f", tmp, dst)
}

// daemonConfig is daemon.json with its proxies set to the gateway, merged into the file's
// other keys when it exists (a proxies key there is replaced). The file is read with sudo -n
// cat, as it's written, so a root-only one works; an absent one is {}.
func (r *setup) daemonConfig(ctx context.Context) ([]byte, error) {
	cfg := map[string]json.RawMessage{}
	raw, err := r.readPriv(ctx, r.path(daemonJSON), maxDaemonJSON)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case len(bytes.TrimSpace(raw)) > 0:
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("it isn't a JSON object: %w", err)
		}
		if cfg == nil {
			return nil, errors.New("it isn't a JSON object: null")
		}
	}
	px, err := json.Marshal(map[string]string{"http-proxy": r.proxy, "https-proxy": r.proxy, "no-proxy": r.noPx})
	if err != nil {
		return nil, err
	}
	cfg["proxies"] = px
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// waitDocker polls docker info until dockerd answers, for DockerTimeout on the clock.
func (r *setup) waitDocker(ctx context.Context) error {
	deadline := r.Clock.Now().Add(r.DockerTimeout)
	for {
		actx, cancel := context.WithTimeout(ctx, dockerAttempt)
		err := r.priv(actx, "docker", "info")
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for dockerd: %w", ctx.Err())
		}
		if !r.Clock.Now().Before(deadline) {
			return fmt.Errorf("dockerd didn't answer within %s: %w", r.DockerTimeout, err)
		}
		t := r.Clock.NewTimer(dockerPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waiting for dockerd: %w", ctx.Err())
		case <-t.C():
		}
	}
}

// readPriv reads the regular file p, of at most limit bytes, as root (sudo -n cat). A p that
// the run's user sees is absent is fs.ErrNotExist, without a command: its directory is
// readable (/etc/docker is 0755) even when the file is root-only.
func (r *setup) readPriv(ctx context.Context, p string, limit int64) ([]byte, error) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err == nil && !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", p)
	case err == nil && fi.Size() > limit:
		return nil, fmt.Errorf("%s is over %d bytes", p, limit)
	}
	// err is nil, or a permission error that sudo may not have: cat says.
	out := &capped{limit: limit}
	if err := r.privOut(ctx, out, "cat", "--", p); err != nil {
		return nil, err
	}
	if out.over {
		return nil, fmt.Errorf("%s is over %d bytes", p, limit)
	}
	return out.b.Bytes(), nil
}

// priv runs a command as root: with sudo -n, or directly when the run is root.
func (r *setup) priv(ctx context.Context, name string, args ...string) error {
	return r.privOut(ctx, nil, name, args...)
}

// privOut is priv with the command's stdout to stdout; nil keeps it with stderr for the error.
func (r *setup) privOut(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	if r.root {
		return r.runOut(ctx, stdout, name, args...)
	}
	return r.runOut(ctx, stdout, "sudo", append([]string{"-n", name}, args...)...)
}

// runOut runs a command with its stdout to stdout; nil keeps it with stderr, whose last
// maxOutputTail bytes the error carries.
func (r *setup) runOut(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	var errOut tail
	if stdout == nil {
		stdout = &errOut
	}
	if err := r.Runner.Run(ctx, stdout, &errOut, name, args...); err != nil {
		line := strings.Join(append([]string{name}, args...), " ")
		if t := strings.TrimSpace(errOut.String()); t != "" {
			return fmt.Errorf("%s: %w: %s", line, err, t)
		}
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
}

// capped keeps the first limit bytes written to it, and whether more came.
type capped struct {
	b     bytes.Buffer
	limit int64
	over  bool
}

// Write takes all of p (a short write would fail the command), keeping what fits.
func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.limit - int64(c.b.Len()); int64(n) > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.b.Write(p)
	return n, nil
}

func (r *setup) path(p string) string { return filepath.Join(r.Root, p) }

// tail keeps the last maxOutputTail bytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - maxOutputTail; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }

// parseCertificates checks that b is one or more PEM CERTIFICATE blocks of CA certificates,
// with nothing but whitespace around and between them (the CA's public half only, never a
// key or other text), and returns them re-encoded: what Setup installs is what it parsed,
// never b's bytes.
func parseCertificates(b []byte) ([]byte, error) {
	var out bytes.Buffer
	for rest := b; ; {
		next := bytes.TrimLeft(rest, " \t\r\n")
		if len(next) == 0 {
			break
		}
		if !bytes.HasPrefix(next, []byte(pemBegin)) {
			if out.Len() == 0 {
				return nil, errors.New("no PEM CERTIFICATE block at its start")
			}
			return nil, errors.New("data between or after its PEM blocks")
		}
		var blk *pem.Block
		blk, rest = pem.Decode(next)
		// pem.Decode skips a block it can't parse (a type line without its closing dashes, an
		// END line that doesn't match) and returns the next one: the block must be the one at
		// next's start, so what it consumed holds no other BEGIN line.
		if blk == nil || bytes.Contains(next[len(pemBegin):len(next)-len(rest)], []byte("\n"+pemBegin)) {
			return nil, errors.New("a PEM block that doesn't decode")
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a PEM %q block: want only CERTIFICATE blocks", blk.Type)
		}
		if len(blk.Headers) > 0 {
			return nil, errors.New("a PEM CERTIFICATE block with headers")
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a PEM CERTIFICATE block that isn't a certificate: %w", err)
		}
		if !cert.BasicConstraintsValid || !cert.IsCA {
			return nil, errors.New("a certificate that isn't a CA")
		}
		out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes}))
	}
	if out.Len() == 0 {
		return nil, errors.New("no PEM CERTIFICATE block")
	}
	return out.Bytes(), nil
}

const pemBegin = "-----BEGIN "

// readCapped reads a regular file of at most limit bytes.
func readCapped(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", p, limit)
	}
	return b, nil
}

// writeFileAtomic writes b to a temp file beside p, then renames it over p.
func writeFileAtomic(p string, b []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Chmod(perm)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp, p)
}
