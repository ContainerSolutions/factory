// Package vmrun is factory-agent run's runtime inside a VM workspace (workspace-run.md). This
// file is its system setup: the environment that points every tool at the gateway (the
// proxies, the CA bundle, the placeholders) and, with --system-setup on, the privileged part
// (the system trust store, dockerd's proxy).
package vmrun

import (
	"bytes"
	"context"
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

// SystemSetup is --system-setup: whether Setup runs its privileged part.
type SystemSetup string

const (
	// SetupAuto is SetupOn in a VM (systemd runs and sudo -n works, or the run is root), else
	// SetupOff (a plain container, a test).
	SetupAuto SystemSetup = "auto"
	// SetupOn installs the gateway CA into the system store and gives dockerd the proxy.
	SetupOn SystemSetup = "on"
	// SetupOff only builds the environment and the files under the state dir.
	SetupOff SystemSetup = "off"
)

// ParseSystemSetup parses --system-setup.
func ParseSystemSetup(s string) (SystemSetup, error) {
	switch m := SystemSetup(s); m {
	case SetupAuto, SetupOn, SetupOff:
		return m, nil
	}
	return "", fmt.Errorf("--system-setup %q: want auto, on or off", s)
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
	systemRoots   = "/etc/ssl/certs/ca-certificates.crt"
	caInstall     = "/usr/local/share/ca-certificates/factory-gateway.crt"
	daemonJSON    = "/etc/docker/daemon.json"
	systemdRunDir = "/run/systemd/system"
)

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

// Runner runs a command; out gets its stdout and stderr.
type Runner interface {
	Run(ctx context.Context, out io.Writer, name string, args ...string) error
}

// ExecRunner runs real programs, found on PATH.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
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
	// Root prefixes the system paths (/etc, /usr/local, /run); "" is /.
	Root string
	// Runner runs sudo, update-ca-certificates, systemctl and docker; default ExecRunner.
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
// bundle and the docker client config are written in every mode; SetupOn (and SetupAuto in a
// VM) also installs the gateway CA into the system store, gives dockerd the proxy and restarts
// it. Every error fails the run environment-error.
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
	ca, err := readCapped(b.CAFile, runbundle.MaxCABytes)
	if err != nil {
		return nil, fmt.Errorf("system setup: the gateway CA: %w", err)
	}
	if err := checkCertificates(ca); err != nil {
		return nil, fmt.Errorf("system setup: the gateway CA: %w", err)
	}
	u, err := s.User()
	if err != nil {
		return nil, fmt.Errorf("system setup: the run's user: %w", err)
	}
	r := &setup{System: s, root: u.Uid == "0", user: u, proxy: proxy.String(), noPx: noProxy(b.Manifest.Gateway.NoProxy)}
	if mode, err = r.resolve(ctx, mode); err != nil {
		return nil, err
	}
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

// resolve turns SetupAuto into SetupOn when systemd runs and the run can be root (it is, or
// sudo -n works), else SetupOff.
func (r *setup) resolve(ctx context.Context, mode SystemSetup) (SystemSetup, error) {
	switch mode {
	case SetupOn, SetupOff:
		return mode, nil
	case SetupAuto:
	default:
		return "", fmt.Errorf("system setup: unknown mode %q", mode)
	}
	if fi, err := os.Stat(r.path(systemdRunDir)); err != nil || !fi.IsDir() {
		return SetupOff, nil
	}
	if !r.root {
		if err := r.run(ctx, "sudo", "-n", "true"); err != nil {
			r.Logger.Info("system setup: sudo -n isn't available; the privileged part is off", "err", err)
			return SetupOff, nil
		}
	}
	return SetupOn, nil
}

// dirs makes the state dir and the work dir when they're absent: 0700, by the run's user
// when their parent is writable, else, with SetupOn, by sudo -n install -d owned by the run's
// user. An existing directory is left as it is (the pipeline checks that the work dir is
// empty); a symlink or a file is refused.
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
	err = os.Mkdir(dir, 0o700)
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
	daemon, err := r.daemonConfig()
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

// install stages content in the state dir and installs it at the system path p (0644, its
// directory 0755).
func (r *setup) install(ctx context.Context, stage, p string, content []byte) error {
	src := filepath.Join(stage, filepath.Base(p))
	if err := os.WriteFile(src, content, 0o600); err != nil {
		return err
	}
	if err := r.priv(ctx, "install", "-d", "-m", "0755", filepath.Dir(r.path(p))); err != nil {
		return err
	}
	return r.priv(ctx, "install", "-m", "0644", src, r.path(p))
}

// daemonConfig is daemon.json with its proxies set to the gateway, merged into the file's
// other keys when it exists (a proxies key there is replaced).
func (r *setup) daemonConfig() ([]byte, error) {
	cfg := map[string]json.RawMessage{}
	raw, err := readCapped(r.path(daemonJSON), maxDaemonJSON)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case len(bytes.TrimSpace(raw)) > 0:
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg == nil {
			return nil, errors.New("it isn't a JSON object")
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

// priv runs a command as root: with sudo -n, or directly when the run is root.
func (r *setup) priv(ctx context.Context, name string, args ...string) error {
	if r.root {
		return r.run(ctx, name, args...)
	}
	return r.run(ctx, "sudo", append([]string{"-n", name}, args...)...)
}

// run runs a command; its error carries the last of its output.
func (r *setup) run(ctx context.Context, name string, args ...string) error {
	var out tail
	if err := r.Runner.Run(ctx, &out, name, args...); err != nil {
		line := strings.Join(append([]string{name}, args...), " ")
		if t := strings.TrimSpace(out.String()); t != "" {
			return fmt.Errorf("%s: %w: %s", line, err, t)
		}
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
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

// checkCertificates refuses content that isn't one or more PEM CERTIFICATE blocks: the CA's
// public half only, never a key.
func checkCertificates(b []byte) error {
	n := 0
	for rest := b; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			if n > 0 && len(bytes.TrimSpace(rest)) > 0 {
				return errors.New("trailing data after its PEM blocks")
			}
			break
		}
		if blk.Type != "CERTIFICATE" {
			return fmt.Errorf("a PEM %q block: want only CERTIFICATE blocks", blk.Type)
		}
		n++
	}
	if n == 0 {
		return errors.New("no PEM CERTIFICATE block")
	}
	return nil
}

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
