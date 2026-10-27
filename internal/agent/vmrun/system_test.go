package vmrun

import (
	"bytes"
	"context"
	_ "embed"
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

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/sudocancel/sudocanceltest"
	"github.com/compliance-framework/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// testCA stands in for the gateway CA's public half: a self-signed P-256 CA certificate whose
// key was thrown away.
//
//go:embed testdata/ca/gateway.pem
var testCA string

// testLeaf is a self-signed certificate that isn't a CA.
//
//go:embed testdata/ca/leaf.pem
var testLeaf string

// testRoots stands in for the system's CA roots, without a final newline.
const testRoots = "-----BEGIN CERTIFICATE-----\nc3lzdGVtLXJvb3Q=\n-----END CERTIFICATE-----"

var (
	adminUser = &user.User{Uid: "1000", Gid: "1000", Username: "admin", HomeDir: "/home/admin"}
	rootUser  = &user.User{Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"}
)

// fakeRunner answers the commands Setup runs, as a VM would, without running any: sudo -n
// runs the rest of its line, install, cat and mv are emulated as root would do them, and the
// others are only recorded. It refuses any path outside the test's directory.
type fakeRunner struct {
	mu    sync.Mutex
	base  string // the test's directory: calls show it as $T
	calls []string
	sudo  error            // sudo -n's refusal (a password is required)
	fail  map[string]error // by command name
	ready int              // docker info answers from this call on (0 and 1: at once)
	infos int
	// beforeMV, when set, runs before mv's rename with its source; an error stops the mv.
	beforeMV func(src string) error
}

func (f *fakeRunner) Run(_ context.Context, stdout, out io.Writer, name string, args ...string) error {
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
	case "cat":
		return f.cat(stdout, out, args)
	case "mv":
		return f.mv(args)
	}
	return fmt.Errorf("unexpected command %s", name)
}

func (f *fakeRunner) inBase(paths ...string) error {
	for _, p := range paths {
		if !strings.HasPrefix(p, f.base+string(filepath.Separator)) {
			return fmt.Errorf("a path outside the test's directory: %s", p)
		}
	}
	return nil
}

// cat emulates cat -- file as root: the file's mode doesn't stop it.
func (f *fakeRunner) cat(stdout, stderr io.Writer, args []string) error {
	if len(args) != 2 || args[0] != "--" {
		return fmt.Errorf("cat %v", args)
	}
	p := args[1]
	if err := f.inBase(p); err != nil {
		return err
	}
	fi, err := os.Stat(p)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cat: %s: No such file or directory\n", p)
		return errors.New("exit status 1")
	}
	_ = os.Chmod(p, fi.Mode().Perm()|0o400)
	defer func() { _ = os.Chmod(p, fi.Mode().Perm()) }()
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

// mv emulates mv -f src dst within one directory: a rename.
func (f *fakeRunner) mv(args []string) error {
	if len(args) != 3 || args[0] != "-f" {
		return fmt.Errorf("mv %v", args)
	}
	if err := f.inBase(args[1], args[2]); err != nil {
		return err
	}
	if filepath.Dir(args[1]) != filepath.Dir(args[2]) {
		return fmt.Errorf("mv across directories: %v", args)
	}
	if f.beforeMV != nil {
		if err := f.beforeMV(args[1]); err != nil {
			return err
		}
	}
	return os.Rename(args[1], args[2])
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
	if err := f.inBase(rest...); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if dir && len(rest) == 1 {
		parent := filepath.Dir(rest[0]) // the nearest existing ancestor: install -d makes the rest
		for _, err := os.Stat(parent); errors.Is(err, fs.ErrNotExist); _, err = os.Stat(parent) {
			parent = filepath.Dir(parent)
		}
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
// commands, the environment and every file written.
func TestSetup(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		mode    SystemSetup
		user    *user.User
		roots   bool
		daemon  string // an existing daemon.json, 0600
		ready   int
		noProxy []string
	}{
		{name: "off", mode: SetupOff, user: adminUser, roots: true, noProxy: []string{"internal.example.com", "localhost"}},
		{name: "on", mode: SetupOn, user: adminUser, roots: true, ready: 3, noProxy: []string{"internal.example.com"},
			daemon: `{"log-driver": "journald", "proxies": {"http-proxy": "http://old:1"}}`},
		{name: "on-as-root", mode: SetupOn, user: rootUser, daemon: "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, tt.user, tt.noProxy...)
			w.run.ready = tt.ready
			if tt.roots {
				mustWrite(t, filepath.Join(w.root, systemRoots), testRoots, 0o644)
			}
			if tt.daemon != "" {
				mustWrite(t, filepath.Join(w.root, daemonJSON), tt.daemon, 0o600)
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
// checks (no reserved name, no git, loader or shell control variable) and the Effector's
// (FACTORY_RUNTIME=workspace, no FACTORY_TOKEN). Its state dir is nested, as the default
// /var/lib/factory-agent/run is: the user makes its parents.
func TestSetupEnvIsPipelineEnv(t *testing.T) {
	t.Parallel()
	w := newWorld(t, adminUser, "internal.example.com")
	w.sys.StateDir = filepath.Join(w.base, "var", "lib", "factory-agent", "run")
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
	if fi, err := os.Stat(w.sys.StateDir); err != nil || fi.Mode().Perm() != 0o700 || len(w.run.calls) != 0 {
		t.Errorf("the state dir %v, %v, calls %q: want a 0700 directory and no command", fi, err, w.run.calls)
	}
	// The Effector's checks too: a known FACTORY_RUNTIME, and no FACTORY_TOKEN in the workspace.
	if _, ok := env[pipeline.EnvToken]; ok {
		t.Errorf("Setup sets %s", pipeline.EnvToken)
	}
	if _, err := effector.FromEnv(func(k string) string { return env[k] }, nil); err != nil {
		t.Errorf("effector.FromEnv refuses Setup's env: %v", err)
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
		proxy string // "": proxyURL; "-": none
		mode  SystemSetup
		prep  func(t *testing.T, w *world)
		want  string
		calls int // the commands run before the failure; -1: don't check
	}{
		{name: "no proxy", proxy: "-", mode: SetupOff, want: "--proxy is required"},
		{name: "an https proxy", proxy: "https://gw:3128", mode: SetupOff, want: "the scheme must be http"},
		{name: "a proxy with a path", proxy: "http://gw:3128/x", mode: SetupOff, want: "only a host and a port"},
		{name: "a proxy without a port", proxy: "http://gw", mode: SetupOff, want: "want http://<host>:<port>"},
		{name: "a proxy with user info", proxy: "http://u:p@gw:3128", mode: SetupOff, want: "user info isn't allowed"},
		{name: "an unknown mode", mode: "maybe", want: `unknown mode "maybe"`},
		{name: "the auto mode", mode: "auto", want: `unknown mode "auto"`, calls: 0},
		{name: "no mode", mode: "", want: `unknown mode ""`},
		{name: "no gateway CA", mode: SetupOff, prep: func(_ *testing.T, w *world) { w.bundle.CAFile = "" }, want: "the bundle has no gateway CA"},
		{name: "a key in the CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, testCA+"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", 0o600)
		}, want: `a PEM "PRIVATE KEY" block`},
		{name: "a CA that isn't PEM", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, "not a cert", 0o600) }, want: "no PEM CERTIFICATE block at its start"},
		{name: "trailing data after the CA", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, testCA+"junk", 0o600) }, want: "data between or after its PEM blocks"},
		{name: "data before the CA", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, "junk\n"+testCA, 0o600) }, want: "no PEM CERTIFICATE block at its start"},
		{name: "data between two CAs", mode: SetupOff, prep: func(t *testing.T, w *world) { mustWrite(t, w.bundle.CAFile, testCA+"junk\n"+testCA, 0o600) }, want: "data between or after"},
		{name: "a CA block that doesn't decode", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, "-----BEGIN CERTIFICATE-----\nAAAA\n", 0o600)
		}, want: "a PEM block that doesn't decode"},
		// pem.Decode skips a malformed block and returns the CERTIFICATE after it.
		{name: "a key whose type line isn't closed, before the CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, "-----BEGIN PRIVATE KEY\nAAAA\n-----END PRIVATE KEY-----\n"+testCA, 0o600)
		}, want: "a PEM block that doesn't decode"},
		{name: "a key whose END line doesn't match, before the CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"+testCA, 0o600)
		}, want: "a PEM block that doesn't decode"},
		{name: "text in a BEGIN line, before the CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, "-----BEGIN junk\nsecret=hunter2\n"+testCA, 0o600)
		}, want: "a PEM block that doesn't decode"},
		{name: "a CERTIFICATE block that isn't a certificate", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, "-----BEGIN CERTIFICATE-----\nZmFrZS1nYXRld2F5LWNh\n-----END CERTIFICATE-----\n", 0o600)
		}, want: "a PEM CERTIFICATE block that isn't a certificate"},
		{name: "a CERTIFICATE block with headers", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, strings.Replace(testCA, "-----\n", "-----\nProc-Type: 4,ENCRYPTED\n\n", 1), 0o600)
		}, want: "a PEM CERTIFICATE block with headers"},
		{name: "a certificate that isn't a CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, testCA+testLeaf, 0o600)
		}, want: "a certificate that isn't a CA"},
		{name: "an oversize CA", mode: SetupOff, prep: func(t *testing.T, w *world) {
			mustWrite(t, w.bundle.CAFile, strings.Repeat("a", runbundle.MaxCABytes+1), 0o600)
		}, want: fmt.Sprintf("ca.pem is over %d bytes", runbundle.MaxCABytes)},
		{name: "a CA that's a directory", mode: SetupOff, prep: func(t *testing.T, w *world) {
			if err := os.Remove(w.bundle.CAFile); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(w.bundle.CAFile, 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: "ca.pem is not a regular file"},
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
		}, want: "/etc/docker/daemon.json: it isn't a JSON object: json: cannot unmarshal array", calls: 5},
		{name: "a daemon.json that isn't JSON", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), "{", 0o644)
		}, want: "it isn't a JSON object: unexpected end of JSON input", calls: 5},
		{name: "a daemon.json that's null", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), "null", 0o644)
		}, want: "it isn't a JSON object: null", calls: 5},
		{name: "a daemon.json that's a directory", mode: SetupOn, prep: func(t *testing.T, w *world) {
			if err := os.MkdirAll(filepath.Join(w.root, daemonJSON), 0o755); err != nil {
				t.Fatal(err)
			}
		}, want: "daemon.json is not a regular file", calls: 4},
		{name: "an oversize daemon.json", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), strings.Repeat(" ", maxDaemonJSON+1), 0o600)
		}, want: fmt.Sprintf("daemon.json is over %d bytes", maxDaemonJSON), calls: 4},
		{name: "cat of daemon.json fails", mode: SetupOn, prep: func(t *testing.T, w *world) {
			mustWrite(t, filepath.Join(w.root, daemonJSON), "{}", 0o600)
			w.run.fail = map[string]error{"cat": errors.New("exit status 1")}
		}, want: "/etc/docker/daemon.json: sudo -n cat -- $T/root/etc/docker/daemon.json: exit status 1: cat: it broke", calls: 5},
		{name: "mv of the CA fails", mode: SetupOn, prep: func(_ *testing.T, w *world) {
			w.run.fail = map[string]error{"mv": errors.New("exit status 1")}
		}, want: "the gateway CA: sudo -n mv -f $T/root/usr/local/share/ca-certificates/.factory-gateway.crt.factory-tmp $T/root/usr/local/share/ca-certificates/factory-gateway.crt: exit status 1", calls: 3},
		{name: "sudo refuses", mode: SetupOn, prep: func(_ *testing.T, w *world) { w.run.sudo = errors.New("exit status 1") },
			want: "the gateway CA: sudo -n install -d -m 0755 $T/root/usr/local/share/ca-certificates: exit status 1: sudo: a password is required", calls: 1},
		{name: "update-ca-certificates fails", mode: SetupOn, prep: func(_ *testing.T, w *world) {
			w.run.fail = map[string]error{"update-ca-certificates": errors.New("exit status 2")}
		}, want: "sudo -n update-ca-certificates: exit status 2: update-ca-certificates: it broke", calls: 4},
		{name: "systemctl fails", mode: SetupOn, prep: func(_ *testing.T, w *world) {
			w.run.fail = map[string]error{"systemctl": errors.New("exit status 5")}
		}, want: "sudo -n systemctl restart docker: exit status 5", calls: 8},
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
			switch proxy {
			case "":
				proxy = proxyURL
			case "-":
				proxy = ""
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
	armed := make(chan struct{})
	go func() { w.clk.BlockUntil(1); close(armed) }() // the first poll's timer
	select {
	case <-armed:
	case err := <-done: // Setup failed before the wait: don't block on a timer that never comes.
		cancel()
		t.Fatalf("Setup = %v before waiting for dockerd", err)
	}
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
			// the state dir's parent is absent too, as /var/lib/factory-agent is in the base.
			w.sys.StateDir, w.sys.WorkDir = filepath.Join(locked, "factory-agent", "run"), filepath.Join(locked, "work")
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
				"sudo -n install -d -o 1000 -g 1000 -m 0700 $T/locked/factory-agent/run",
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
	}{{"on", SetupOn, false}, {"off", SetupOff, false}, {"auto", "", true}, {"", "", true}, {"ON", "", true}} {
		got, err := ParseSystemSetup(tt.in)
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("ParseSystemSetup(%q) = %q, %v", tt.in, got, err)
		}
	}
	if _, err := ParseSystemSetup("auto"); err == nil || !strings.Contains(err.Error(), "there is no automatic mode; pass on in a workspace VM") {
		t.Errorf("ParseSystemSetup(auto) = %v, want it to say there is no automatic mode", err)
	}
	if DefaultSystemSetup != SetupOff {
		t.Errorf("DefaultSystemSetup = %q, want off", DefaultSystemSetup)
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
	if err := (ExecRunner{}).Run(context.Background(), &out, &out, "/bin/sh", "-c", "echo out; echo err >&2"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out\nerr\n" {
		t.Errorf("output %q", out.String())
	}
	var stdout, stderr bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), &stdout, &stderr, "/bin/sh", "-c", "echo out; echo err >&2"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	err := (ExecRunner{}).Run(context.Background(), io.Discard, io.Discard, "/bin/sh", "-c", "exit 3")
	if err == nil || !strings.HasPrefix(err.Error(), "/bin/sh: exit status 3") {
		t.Errorf("Run = %v, want /bin/sh's exit status", err)
	}
}

// TestExecRunnerCancel: when ctx ends, a sudo call (setup's sudo -n commands) gets SIGTERM,
// which sudo relays, and SIGKILL only after Grace; anything else is killed.
func TestExecRunnerCancel(t *testing.T) {
	t.Parallel()
	r := ExecRunner{Grace: 2 * time.Second}
	sudocanceltest.Check(t, r.Grace, func(ctx context.Context, prog string, args ...string) error {
		return r.Run(ctx, io.Discard, io.Discard, prog, args...)
	})
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

// TestSetupDaemonJSON: in SetupOn, daemon.json is read with sudo -n cat (a root-only file
// works), an absent one is {} without a command, and the file written keeps the mode of the
// one it replaces (0644 when there was none).
func TestSetupDaemonJSON(t *testing.T) {
	t.Parallel()
	wantProxies := `"proxies": {
    "http-proxy": "http://192.168.64.1:3128",`
	for _, tt := range []struct {
		name     string
		existing string // "": none
		mode     fs.FileMode
		want     []string // in the file written
		wantMode fs.FileMode
	}{
		{name: "missing", want: []string{"{\n  " + wantProxies}, wantMode: 0o644},
		{name: "root-only", existing: `{"data-root": "/srv/docker"}`, mode: 0o000, want: []string{`"data-root": "/srv/docker"`, wantProxies}, wantMode: 0o000},
		{name: "0640", existing: `{"debug": true}`, mode: 0o640, want: []string{`"debug": true`, wantProxies}, wantMode: 0o640},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, adminUser)
			p := filepath.Join(w.root, daemonJSON)
			if tt.existing != "" {
				mustWrite(t, p, tt.existing, 0o600)
				if err := os.Chmod(p, tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.setup(t, proxyURL, SetupOn); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			cat := "sudo -n cat -- $T/root/etc/docker/daemon.json"
			if got := slices.Contains(w.run.calls, cat); got != (tt.existing != "") {
				t.Errorf("calls %q: %q there is %v, want %v", w.run.calls, cat, got, !got)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != tt.wantMode {
				t.Errorf("daemon.json's mode %04o, want %04o", fi.Mode().Perm(), tt.wantMode)
			}
			_ = os.Chmod(p, 0o600)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(string(b), want) {
					t.Errorf("daemon.json\n%s\nwant it to contain %q", b, want)
				}
			}
		})
	}
}

// TestSetupInstallIsAtomic: a system file is installed as a temp file beside it, then renamed
// over it with mv -f, so a failed step leaves the old file whole.
func TestSetupInstallIsAtomic(t *testing.T) {
	t.Parallel()
	w := newWorld(t, adminUser)
	p := filepath.Join(w.root, daemonJSON)
	const old = `{"debug": true}`
	mustWrite(t, p, old, 0o600)
	w.run.fail = map[string]error{"systemctl": errors.New("stop here")}
	failMV := true
	w.run.beforeMV = func(src string) error {
		if strings.HasSuffix(src, "/.daemon.json"+tmpSuffix) && failMV {
			// the temp file is complete before the rename; daemon.json is still the old one.
			if b, err := os.ReadFile(p); err != nil || string(b) != old {
				t.Errorf("daemon.json before the rename: %q, %v", b, err)
			}
			if b, err := os.ReadFile(src); err != nil || !strings.Contains(string(b), "proxies") {
				t.Errorf("the temp file before the rename: %q, %v", b, err)
			}
			return errors.New("killed")
		}
		return nil
	}
	if _, err := w.setup(t, proxyURL, SetupOn); err == nil || !strings.Contains(err.Error(), "daemon.json: killed") {
		t.Fatalf("Setup = %v, want the mv to fail", err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != old {
		t.Errorf("daemon.json after a failed rename: %q, %v, want the old file", b, err)
	}
	want := []string{
		"sudo -n install -d -m 0755 $T/root/etc/docker",
		"sudo -n install -m 0600 $T/state/system/daemon.json $T/root/etc/docker/.daemon.json" + tmpSuffix,
		"sudo -n mv -f $T/root/etc/docker/.daemon.json" + tmpSuffix + " $T/root/etc/docker/daemon.json",
	}
	if n := len(w.run.calls); n < 3 || !slices.Equal(w.run.calls[n-3:], want) {
		t.Errorf("calls %q, want them to end with %q", w.run.calls, want)
	}

	// The same run with the rename let through replaces the file and leaves no temp file.
	failMV = false
	w.run.calls = nil
	if _, err := w.setup(t, proxyURL, SetupOn); err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("Setup = %v, want it to stop at systemctl", err)
	}
	if b, err := os.ReadFile(p); err != nil || !strings.Contains(string(b), "proxies") || !strings.Contains(string(b), `"debug": true`) {
		t.Errorf("daemon.json %q, %v: want the merged file", b, err)
	}
	for _, d := range []string{filepath.Dir(p), filepath.Dir(filepath.Join(w.root, caInstall))} {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), tmpSuffix) {
				t.Errorf("%s left in %s", e.Name(), d)
			}
		}
	}
}

// TestParseCertificates: what Setup installs is the certificates re-encoded, never the input's
// bytes (here CRLF line ends and blank lines between the blocks).
func TestParseCertificates(t *testing.T) {
	t.Parallel()
	in := "\r\n" + strings.ReplaceAll(testCA, "\n", "\r\n") + "\n\n" + testCA + "  \n"
	got, err := parseCertificates([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := testCA + testCA; string(got) != want {
		t.Errorf("parseCertificates = %q, want %q", got, want)
	}
}

// TestCapped keeps the first limit bytes, reports more, and never writes short.
func TestCapped(t *testing.T) {
	t.Parallel()
	c := &capped{limit: 4}
	for _, p := range []string{"ab", "cde", "f"} {
		if n, err := c.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if c.b.String() != "abcd" || !c.over {
		t.Errorf("capped %q, over %v: want abcd, true", c.b.String(), c.over)
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
