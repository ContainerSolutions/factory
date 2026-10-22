package vmrun

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// testCA stands in for the gateway CA's public half.
const testCA = "-----BEGIN CERTIFICATE-----\nZmFrZS1nYXRld2F5LWNh\n-----END CERTIFICATE-----\n"

// testRoots stands in for the system's CA roots, without a final newline.
const testRoots = "-----BEGIN CERTIFICATE-----\nc3lzdGVtLXJvb3Q=\n-----END CERTIFICATE-----"

var (
	adminUser = &user.User{Uid: "1000", Gid: "1000", Username: "admin", HomeDir: "/home/admin"}
	rootUser  = &user.User{Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"}
)

// fakeRunner answers the commands Setup runs, as a VM would, without running any: sudo -n
// runs the rest of its line, install is emulated as root would do it, and the others are only
// recorded. It refuses any path outside the test's directory.
type fakeRunner struct {
	mu    sync.Mutex
	base  string // the test's directory: calls show it as $T
	calls []string
	sudo  error            // sudo -n's refusal (a password is required)
	fail  map[string]error // by command name
	ready int              // docker info answers from this call on (0 and 1: at once)
	infos int
}

func (f *fakeRunner) Run(_ context.Context, out io.Writer, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.ReplaceAll(strings.Join(append([]string{name}, args...), " "), f.base, "$T"))
	if name == "sudo" {
		if len(args) < 2 || args[0] != "-n" {
			return fmt.Errorf("sudo without -n: %v", args)
		}
		if f.sudo != nil {
			_, _ = io.WriteString(out, "sudo: a password is required\n")
			return f.sudo
		}
		name, args = args[1], args[2:]
	}
	if err := f.fail[name]; err != nil {
		_, _ = fmt.Fprintf(out, "%s: it broke\n", name)
		return err
	}
	switch name {
	case "true", "update-ca-certificates", "systemctl":
		return nil
	case "docker":
		f.infos++
		if f.infos < f.ready {
			_, _ = io.WriteString(out, "Cannot connect to the Docker daemon\n")
			return errors.New("exit status 1")
		}
		return nil
	case "install":
		return f.install(args)
	}
	return fmt.Errorf("unexpected command %s", name)
}

// install emulates install -d [-o u -g g] -m mode dir and install -m mode src dst as root:
// the parent's permissions don't stop it.
func (f *fakeRunner) install(args []string) error {
	dir, mode, rest := false, fs.FileMode(0o755), []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-d":
			dir = true
		case "-o", "-g":
			i++
		case "-m":
			i++
			var m uint32
			if _, err := fmt.Sscanf(args[i], "%o", &m); err != nil {
				return err
			}
			mode = fs.FileMode(m)
		default:
			rest = append(rest, args[i])
		}
	}
	for _, p := range rest {
		if !strings.HasPrefix(p, f.base+string(filepath.Separator)) {
			return fmt.Errorf("install outside the test's directory: %s", p)
		}
	}
	if dir && len(rest) == 1 {
		parent := filepath.Dir(rest[0])
		if fi, err := os.Stat(parent); err == nil {
			_ = os.Chmod(parent, fi.Mode().Perm()|0o700)
			defer func() { _ = os.Chmod(parent, fi.Mode().Perm()) }()
		}
		if err := os.MkdirAll(rest[0], mode); err != nil {
			return err
		}
		return os.Chmod(rest[0], mode)
	}
	if dir || len(rest) != 2 {
		return fmt.Errorf("install %v", args)
	}
	b, err := os.ReadFile(rest[0])
	if err != nil {
		return err
	}
	if err := os.WriteFile(rest[1], b, mode); err != nil {
		return err
	}
	return os.Chmod(rest[1], mode)
}

// tick advances clk by a second whenever a timer waits, until stop is closed.
func tick(clk *clock.Fake, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if clk.Timers() > 0 {
			clk.Advance(time.Second)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
}

type world struct {
	base, root, state, work string
	bundle                  *runbundle.Bundle
	run                     *fakeRunner
	clk                     *clock.Fake
	sys                     System
}

// newWorld lays out a test VM under t.TempDir(): $T/root is the system root, $T/state the
// state dir, $T/bundle the bundle with its CA.
func newWorld(t *testing.T, u *user.User, noProxy ...string) *world {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{base: base, root: filepath.Join(base, "root"), state: filepath.Join(base, "state"), work: filepath.Join(base, "work")}
	ca := filepath.Join(base, "bundle", runbundle.CAFile)
	mustWrite(t, ca, testCA, 0o444)
	w.bundle = &runbundle.Bundle{Dir: filepath.Join(base, "bundle"), CAFile: ca, Manifest: runbundle.Manifest{Gateway: runbundle.Gateway{CAFile: runbundle.CAFile, NoProxy: noProxy}}}
	if err := os.MkdirAll(w.root, 0o755); err != nil {
		t.Fatal(err)
	}
	w.run = &fakeRunner{base: base}
	w.clk = clock.NewFake(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC))
	w.sys = System{
		StateDir: w.state, WorkDir: w.work, Root: w.root, Runner: w.run, Clock: w.clk,
		User:   func() (*user.User, error) { return u, nil },
		Logger: slog.New(slog.DiscardHandler),
	}
	return w
}

func (w *world) setup(t *testing.T, proxy string, mode SystemSetup) (map[string]string, error) {
	t.Helper()
	var u *url.URL
	if proxy != "" {
		var err error
		if u, err = url.Parse(proxy); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	defer close(stop)
	go tick(w.clk, stop)
	return w.sys.Setup(context.Background(), w.bundle, u, mode)
}

func mustWrite(t *testing.T, p, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p) // the bundle's CA is read-only
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

const proxyURL = "http://192.168.64.1:3128"

// TestSetup runs Setup in each mode against a temp root and pins, in a golden per case, the
// resolved mode, the commands, the environment and every file written.
func TestSetup(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		mode    SystemSetup
		user    *user.User
		systemd bool
		sudo    error
		roots   bool
		daemon  string // an existing daemon.json
		ready   int
		noProxy []string
	}{
		{name: "off", mode: SetupOff, user: adminUser, roots: true, noProxy: []string{"internal.example.com", "localhost"}},
		{name: "on", mode: SetupOn, user: adminUser, roots: true, ready: 3, noProxy: []string{"internal.example.com"},
			daemon: `{"log-driver": "journald", "proxies": {"http-proxy": "http://old:1"}}`},
		{name: "on-as-root", mode: SetupOn, user: rootUser},
		{name: "auto-vm", mode: SetupAuto, user: adminUser, systemd: true, roots: true},
		{name: "auto-no-systemd", mode: SetupAuto, user: adminUser, roots: true},
		{name: "auto-no-sudo", mode: SetupAuto, user: adminUser, systemd: true, sudo: errors.New("exit status 1"), roots: true},
		{name: "auto-root", mode: SetupAuto, user: rootUser, systemd: true, roots: true, daemon: "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, tt.user, tt.noProxy...)
			w.run.sudo, w.run.ready = tt.sudo, tt.ready
			if tt.systemd {
				if err := os.MkdirAll(filepath.Join(w.root, systemdRunDir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.roots {
				mustWrite(t, filepath.Join(w.root, systemRoots), testRoots, 0o644)
			}
			if tt.daemon != "" {
				mustWrite(t, filepath.Join(w.root, daemonJSON), tt.daemon, 0o644)
			}
			env, err := w.setup(t, proxyURL, tt.mode)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			got := w.report(t, env)
			golden(t, filepath.Join("testdata", "setup", tt.name+".golden"), got)

			if b, err := os.ReadFile(filepath.Join(env["DOCKER_CONFIG"], "config.json")); err != nil || bytes.Contains(b, []byte("prox")) {
				t.Errorf("the docker client config %q, %v: want no proxies", b, err)
			}
		})
	}
}

// report is the case's golden: the calls, the environment, then every file under $T but the
// bundle, with its mode.
func (w *world) report(t *testing.T, env map[string]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# calls\n")
	for _, c := range w.run.calls {
		b.WriteString(c + "\n")
	}
	b.WriteString("# env\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "%s=%s\n", k, strings.ReplaceAll(env[k], w.base, "$T"))
	}
	b.WriteString("# files\n")
	err := filepath.WalkDir(w.base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p == w.bundle.Dir {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		c, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(w.base, p)
		fmt.Fprintf(&b, "== %s %04o\n%s\n", filepath.ToSlash(rel), fi.Mode().Perm(), strings.TrimRight(string(c), "\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func golden(t *testing.T, p, got string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs (run with -update):\n--- got\n%s\n--- want\n%s", p, got, want)
	}
}

// TestSetupEnvIsPipelineEnv: every variable Setup returns passes pipeline.Options.Env's
// checks (no reserved name, no git, loader or shell control variable).
func TestSetupEnvIsPipelineEnv(t *testing.T) {
	t.Parallel()
	w := newWorld(t, adminUser, "internal.example.com")
	env, err := w.setup(t, proxyURL, SetupOff)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.New(pipeline.Options{
		Root: w.work, GitAuth: pipeline.GitAuthGateway, Env: env,
		Adapters: map[string]harness.Adapter{fake.Name: &fake.Adapter{}}, AgentBinary: "/usr/local/bin/factory-agent",
	})
	if err != nil {
		t.Fatalf("pipeline.New refuses Setup's env: %v", err)
	}
	if env["GH_TOKEN"] != harness.Placeholder || env[pipeline.EnvRuntime] != pipeline.RuntimeWorkspace {
		t.Errorf("GH_TOKEN %q, %s %q", env["GH_TOKEN"], pipeline.EnvRuntime, env[pipeline.EnvRuntime])
	}
}

// TestSetupErrors: every refusal and every failed step is an error naming what failed, and
// SetupOn stops at the first failed command.
func TestSetupErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		proxy string
		mode  SystemSetup
		prep  func(t *testing.T, w *world)
		want  string
		calls int // the commands run before the failure; -1: don't check
	}{
		{name: "no proxy", mode: SetupOff, want: "--proxy is required"},
		{name: "an https proxy", proxy: "https://gw:3128", mode: SetupOff, want: "the scheme must be http"},
		{name: "a proxy with a path", proxy: "http://gw:3128/x", mode: SetupOff, want: "only a host and a port"},
		{name: "a proxy without a port", proxy: "http://gw", mode: SetupOff, want: "want http://<host>:<port>"},
		{name: "a proxy with user info", proxy: "http://u:p@gw:3128", mode: SetupOff, want: "user info isn't allowed"},
		{name: "an unknown mode", mode: "maybe", want: `unknown mode "maybe"`},
		{name: "no gateway CA", mode: SetupOff, prep: func(_ *testing.T, w *world) { w.bundle.CAFile = "" }, want: "the bundle has no gateway CA"},
		{name: "a key in the CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, testCA+"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", 0o600)
		}, want: `a PEM "PRIVATE KEY" block`},
		{name: "a CA that isn't PEM", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, "not a cert", 0o600) }, want: "no PEM CERTIFICATE block"},
		{name: "trailing data after the CA", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, testCA+"junk", 0o600) }, want: "trailing data"},
		{name: "a relative state dir", mode: SetupOff, prep: func(_ *testing.T, w *world) { w.sys.StateDir = "state" }, want: `the state dir "state" must be absolute`},
		{name: "a relative work dir", mode: SetupOff, prep: func(_ *testing.T, w *world) { w.sys.WorkDir = "work" }, want: `the work dir "work" must be absolute`},
		{name: "the user lookup fails", mode: SetupOff, prep: func(_ *testing.T, w *world) {
			w.sys.User = func() (*user.User, error) { return nil, errors.New("no passwd entry") }
		}, want: "the run's user: no passwd entry"},
		{name: "a state dir that's a symlink", mode: SetupOff, prep: func(t *testing.T, w *world) {
			if err := os.Symlink(w.root, w.state); err != nil {
				t.Fatal(err)
			}
		}, want: "the state dir: " + "$T/state exists and is not a directory"},
		{name: "a daemon.json that's an array", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), "[]", 0o644)
		}, want: "/etc/docker/daemon.json: it isn't a JSON object", calls: 3},
		{name: "a daemon.json that's null", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), "null", 0o644)
		}, want: "it isn't a JSON object", calls: 3},
		{name: "sudo refuses", mode: SetupOn, prep: func(_ *testing.T, w *world) { w.run.sudo = errors.New("exit status 1") },
			want: "the gateway CA: sudo -n install -d -m 0755 $T/root/usr/local/share/ca-certificates: exit status 1: sudo: a password is required", calls: 1},
		{name: "update-ca-certificates fails", mode: SetupOn, prep: func(_ *testing.T, w *world) {
			w.run.fail = map[string]error{"update-ca-certificates": errors.New("exit status 2")}
		}, want: "sudo -n update-ca-certificates: exit status 2: update-ca-certificates: it broke", calls: 3},
		{name: "systemctl fails", mode: SetupOn, prep: func(_ *testing.T, w *world) {
			w.run.fail = map[string]error{"systemctl": errors.New("exit status 5")}
		}, want: "sudo -n systemctl restart docker: exit status 5", calls: 6},
		{name: "dockerd never answers", mode: SetupOn, prep: func(_ *testing.T, w *world) { w.run.ready = 1 << 30 },
			want: "dockerd didn't answer within 1m0s: sudo -n docker info: exit status 1: Cannot connect to the Docker daemon", calls: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, adminUser)
			if tt.prep != nil {
				tt.prep(t, w)
			}
			proxy := tt.proxy
			if proxy == "" && tt.name != "no proxy" {
				proxy = proxyURL
			}
			env, err := w.setup(t, proxy, tt.mode)
			if err == nil {
				t.Fatalf("Setup = %v, want an error", env)
			}
			msg := strings.ReplaceAll(err.Error(), w.base, "$T")
			if !strings.Contains(msg, tt.want) || !strings.HasPrefix(msg, "system setup: ") {
				t.Errorf("error %q, want it to contain %q", msg, tt.want)
			}
			if tt.calls >= 0 && len(w.run.calls) != tt.calls {
				t.Errorf("calls %q, want %d", w.run.calls, tt.calls)
			}
		})
	}
}

// TestSetupDockerWaitCancelled: a cancelled context stops the wait for dockerd.
func TestSetupDockerWaitCancelled(t *testing.T) {
	t.Parallel()
	w := newWorld(t, adminUser)
	w.run.ready = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- errOnly(w.sys.Setup(ctx, w.bundle, mustURL(t, proxyURL), SetupOn)) }()
	w.clk.BlockUntil(1) // the first poll's timer
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Setup = %v, want context.Canceled", err)
	}
}

// TestSetupMakesDirsWithSudo: a state or work dir whose parent the run's user can't write is
// made with sudo -n install -d, owned by the user, in SetupOn only.
func TestSetupMakesDirsWithSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes any directory")
	}
	t.Parallel()
	for _, mode := range []SystemSetup{SetupOn, SetupOff} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, adminUser)
			locked := filepath.Join(w.base, "locked")
			if err := os.Mkdir(locked, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			w.sys.StateDir, w.sys.WorkDir = filepath.Join(locked, "state"), filepath.Join(locked, "work")
			// keep the privileged part to the dirs: dockerd and the CA are TestSetup's.
			w.run.fail = map[string]error{"update-ca-certificates": errors.New("stop here")}
			_, err := w.setup(t, proxyURL, mode)
			if mode == SetupOff {
				if !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("Setup = %v, want a permission error", err)
				}
				return
			}
			want := []string{
				"sudo -n install -d -o 1000 -g 1000 -m 0700 $T/locked/state",
				"sudo -n install -d -o 1000 -g 1000 -m 0700 $T/locked/work",
			}
			if len(w.run.calls) < 2 || !slices.Equal(w.run.calls[:2], want) {
				t.Errorf("calls %q, want them to start with %q", w.run.calls, want)
			}
			for _, d := range []string{w.sys.StateDir, w.sys.WorkDir} {
				if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("%s: %v %v, want a 0700 directory", d, fi, err)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "stop here") {
				t.Errorf("Setup = %v, want it to stop at update-ca-certificates", err)
			}
		})
	}
}

func TestParseSystemSetup(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want SystemSetup
		err  bool
	}{{"auto", SetupAuto, false}, {"on", SetupOn, false}, {"off", SetupOff, false}, {"", "", true}, {"ON", "", true}} {
		got, err := ParseSystemSetup(tt.in)
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("ParseSystemSetup(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestParseProxy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in, want string // want "": an error
	}{
		{"http://192.168.64.1:3128", "http://192.168.64.1:3128"},
		{"http://gw.local:8080/", "http://gw.local:8080/"},
		{"http://[fd00::1]:3128", "http://[fd00::1]:3128"},
		{"http://gw:3128?x=1", ""},
		{"http://gw:3128#f", ""},
		{"socks5://gw:1080", ""},
		{"gw:3128", ""},
		{"http://:3128", ""},
		{"", ""},
	} {
		u, err := ParseProxy(tt.in)
		switch {
		case tt.want == "" && err == nil:
			t.Errorf("ParseProxy(%q) = %v, want an error", tt.in, u)
		case tt.want != "" && (err != nil || u.String() != tt.want):
			t.Errorf("ParseProxy(%q) = %v, %v, want %s", tt.in, u, err, tt.want)
		}
	}
}

// TestExecRunner runs a real program (sh, never a privileged one) and keeps its output.
func TestExecRunner(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), &out, "/bin/sh", "-c", "echo out; echo err >&2"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out\nerr\n" {
		t.Errorf("output %q", out.String())
	}
	err := (ExecRunner{}).Run(context.Background(), io.Discard, "/bin/sh", "-c", "exit 3")
	if err == nil || !strings.HasPrefix(err.Error(), "/bin/sh: exit status 3") {
		t.Errorf("Run = %v, want /bin/sh's exit status", err)
	}
}

// TestTail keeps only the last maxOutputTail bytes.
func TestTail(t *testing.T) {
	t.Parallel()
	var tl tail
	_, _ = tl.Write(bytes.Repeat([]byte("a"), maxOutputTail))
	_, _ = tl.Write([]byte("bc"))
	if s := tl.String(); len(s) != maxOutputTail || !strings.HasSuffix(s, "abc") {
		t.Errorf("tail of %d bytes ending %q", len(s), s[len(s)-3:])
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func errOnly(_ map[string]string, err error) error { return err }
