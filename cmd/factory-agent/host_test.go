package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/host"
)

// macRunner answers like a Mac with Tart on PATH; any other command (sudo, a download's
// codesign) fails the test.
type macRunner struct {
	t     *testing.T
	calls []string
}

func (r *macRunner) Run(_ context.Context, out io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, name)
	if name == "/t/tart" && len(args) > 0 && args[0] == "list" {
		_, _ = io.WriteString(out, `[{"Name":"factory-agent-base-dev","State":"stopped","Source":"local"},{"Name":"factory-agent-warm","State":"stopped","Source":"local"}]`)
		return nil
	}
	answers := map[string]string{
		"sw_vers": "26.6.2", "sysctl": "25769803776", "launchctl": "",
		"df":      "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk3s5 1 1 98351052 79% /",
		"/t/tart": host.TartVersion,
	}
	a, ok := answers[name]
	if !ok {
		r.t.Errorf("unexpected command %s %v", name, args)
		return errors.New("unexpected")
	}
	_, _ = io.WriteString(out, a+"\n")
	return nil
}

type noFetch struct{ t *testing.T }

func (f noFetch) Fetch(context.Context, string, string) error {
	f.t.Error("unexpected download")
	return errors.New("no network")
}

func hostTestDeps(t *testing.T) (hostDeps, *macRunner) {
	r := &macRunner{t: t}
	home := t.TempDir()
	return hostDeps{
		Config: config.Options{Home: func() (string, error) { return home, nil }, GOOS: "darwin"},
		Env: host.Env{
			Runner: r, Fetcher: noFetch{t}, Root: t.TempDir(), Home: home, GOOS: "darwin", GOARCH: "arm64",
			LookPath: func(string) (string, error) { return "/t/tart", nil },
			Prompt:   func(string) (bool, error) { t.Error("unexpected prompt"); return false, nil },
		},
	}, r
}

func TestHostUsage(t *testing.T) {
	tests := []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage: factory-agent host"},
		{[]string{"help"}, 0, "usage: factory-agent host"},
		{[]string{"fix"}, 2, `unknown op "fix"`},
		{[]string{"check", "--dry-run"}, 2, "unexpected arguments or flags"},
		{[]string{"check", "--only", "basevm"}, 2, "unexpected arguments or flags"},
		{[]string{"check", "--keep-failed"}, 2, "unexpected arguments or flags"},
		{[]string{"init", "--remove-pf", "--only", "pf"}, 2, "unexpected arguments or flags"},
		{[]string{"init", "--dry-run", "--only", "basevm,nope"}, 1, `--only: no step "nope"`},
		{[]string{"init", "--json"}, 2, "unexpected arguments or flags"},
		{[]string{"init", "--remove-pf", "--yes"}, 2, "unexpected arguments or flags"},
		{[]string{"check", "extra"}, 2, "unexpected arguments or flags"},
		{[]string{"check", "--driver", "docker"}, 2, `--driver "docker" is not tart or kata`},
		{[]string{"check", "--config", "/nonexistent/agent.yaml"}, 1, "read config"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			d, _ := hostTestDeps(t)
			var out, errOut bytes.Buffer
			if code := runHost(context.Background(), tt.args, &out, &errOut, d); code != tt.code || !strings.Contains(out.String()+errOut.String(), tt.out) {
				t.Errorf("code %d, want %d; output %q, want %q", code, tt.code, out.String()+errOut.String(), tt.out)
			}
		})
	}
}

func TestHostCheckAndDryRun(t *testing.T) {
	d, r := hostTestDeps(t)
	var out, errOut bytes.Buffer
	if code := runHost(context.Background(), []string{"check", "--json"}, &out, &errOut, d); code != 1 {
		t.Fatalf("check: code %d, %s", code, errOut.String())
	}
	var sts []host.Status
	if err := json.Unmarshal(out.Bytes(), &sts); err != nil || len(sts) != 7 || sts[2].State != host.StateOK || sts[3].State != host.StateMissing ||
		sts[5].State != host.StateWrong || !strings.Contains(sts[5].Detail, "build it with make tart-up") {
		t.Fatalf("check --json: %v %s", err, out.String())
	}

	out.Reset()
	if code := runHost(context.Background(), []string{"init", "--dry-run"}, &out, &errOut, d); code != 0 {
		t.Fatalf("init --dry-run: code %d, %s", code, errOut.String())
	}
	for _, w := range []string{"plan:\n  statedir  create", "sudo script (one sudo prompt):\n#!/bin/sh", "softnet   missing"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("dry run output lacks %q:\n%s", w, out.String())
		}
	}
	for _, c := range r.calls {
		if c == "sudo" {
			t.Error("--dry-run ran sudo")
		}
	}
	if _, err := os.Stat(filepath.Join(d.Env.Home, "Library")); err == nil {
		t.Error("--dry-run made the state dir")
	}

	// --only basevm,warmbase with --agent-linux: their plan, and the missing state dir's (they
	// write there), no sudo script.
	agent := filepath.Join(t.TempDir(), "agent-linux")
	if err := os.WriteFile(agent, []byte("linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runHost(context.Background(), []string{"init", "--dry-run", "--only", "basevm, warmbase", "--agent-linux", agent}, &out, &errOut, d); code != 0 {
		t.Fatalf("init --dry-run --only: code %d, %s", code, errOut.String())
	}
	for w, want := range map[string]bool{"basevm    tart clone ghcr.io/cirruslabs/ubuntu@sha256:": true, "copy " + agent: true, "warmbase  tart clone factory-agent-base-dev": true, "statedir  create": true, "\n  softnet   ": false, "sudo script": false} {
		if strings.Contains(out.String(), w) != want {
			t.Errorf("--only dry run: %q in the output is %v, want %v:\n%s", w, !want, want, out.String())
		}
	}

	out.Reset()
	if code := runHost(context.Background(), []string{"init", "--remove-pf", "--dry-run"}, &out, &errOut, d); code != 0 || !strings.Contains(out.String(), "/bin/rm -f") {
		t.Fatalf("--remove-pf --dry-run: code %d, %s", code, out.String())
	}
}

// TestHostCheckOK is a host where every step is ok: check exits 0, and init has nothing to do.
func TestHostCheckOK(t *testing.T) {
	d, _ := hostTestDeps(t)
	e := &d.Env
	bin := []byte("softnet")
	e.Pins = host.DefaultPins()
	sum := sha256.Sum256(bin)
	e.Pins.SoftnetBinarySHA256 = hex.EncodeToString(sum[:])
	e.Owner = func(fs.FileInfo) (int, int, bool) { return 0, 0, true }
	e.UID = func() int { return 0 }
	if err := os.MkdirAll(filepath.Join(e.Home, "Library", "Application Support", "factory-agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(e.Home, "agent.yaml")
	write := func(p string, b []byte, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, []byte("workspace: {gatewayPort: 4000}\n"), 0o600)
	soft := filepath.Join(e.Root, host.SoftnetPath)
	write(soft, bin, fs.ModeSetuid|0o755)
	write(filepath.Join(e.Root, host.PFAnchorPath), []byte(host.PFRules(4000)), 0o644)
	write(filepath.Join(e.Root, host.PFConfPath), []byte(`anchor "`+host.PFAnchorName+`"`+"\n"), 0o644)
	write(filepath.Join(e.Root, host.PFPlistPath), []byte(host.PFPlist(filepath.Join(e.Root, host.PFAnchorPath), filepath.Join(e.Root, host.PFConfPath))), 0o644)
	// the base and warm VMs: the linux binary, the records, and tart list (macRunner).
	hex256 := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	e.AgentLinux = filepath.Join(e.Home, host.AgentLinuxName)
	write(e.AgentLinux, []byte("linux"), 0o755)
	state := filepath.Join(e.Home, "Library", "Application Support", "factory-agent")
	base := host.BaseVMRecord{Name: "factory-agent-base-dev", Source: host.BaseImage, ScriptSHA256: host.ProvisionScriptSHA256(), AgentSHA256: hex256([]byte("linux")), AgentVersion: "dev", BuiltAt: "2026-10-22T12:00:00Z"}
	warm := host.WarmBaseRecord{Base: base.Name, BaseBuiltAt: base.BuiltAt, ImagesSHA256: hex256(nil), AgentSHA256: base.AgentSHA256, BuiltAt: base.BuiltAt}
	for name, v := range map[string]any{"basevm.json": base, "warmbase.json": warm} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(state, name), b, 0o600)
	}

	var out, errOut bytes.Buffer
	if code := runHost(context.Background(), []string{"check", "--config", cfg}, &out, &errOut, d); code != 0 {
		t.Fatalf("check: code %d\n%s%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := runHost(context.Background(), []string{"init", "--config", cfg}, &out, &errOut, d); code != 0 || !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("init: code %d\n%s%s", code, out.String(), errOut.String())
	}

	// pf opted out instead of installed: check still exits 0, with a warning.
	if err := os.Remove(filepath.Join(e.Root, host.PFAnchorPath)); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(e.Home, "Library", "Application Support", "factory-agent", "pf.optout"), []byte("date=2026-10-22T09:30:00Z\nreason=--no-pf\n"), 0o600)
	out.Reset()
	errOut.Reset()
	if code := runHost(context.Background(), []string{"check", "--config", cfg}, &out, &errOut, d); code != 0 ||
		!strings.Contains(out.String(), "pf        skipped  opted out on 2026-10-22T09:30:00Z (--no-pf, ") ||
		!strings.Contains(errOut.String(), "warning: pf skipped: opted out") {
		t.Fatalf("check with the pf opt-out: code %d\n%s%s", code, out.String(), errOut.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestLinePrompt(t *testing.T) {
	tests := []struct {
		name    string
		in      io.Reader
		yes     bool
		wantErr bool
	}{
		{"y", strings.NewReader("y\n"), true, false},
		{"YES", strings.NewReader("YES\n"), true, false},
		{"spaces", strings.NewReader("  yes \n"), true, false},
		{"no", strings.NewReader("n\n"), false, false},
		{"other", strings.NewReader("sure\n"), false, false},
		{"empty line", strings.NewReader("\n"), false, false},
		{"EOF without a newline", strings.NewReader("y"), true, false},
		{"nothing", strings.NewReader(""), false, false},
		{"read error", errReader{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			yes, err := linePrompt(tt.in, &out)("Install? [y/N] ")
			if yes != tt.yes || (err != nil) != tt.wantErr || out.String() != "Install? [y/N] " {
				t.Errorf("yes %v err %v out %q; want yes %v, error %v", yes, err, out.String(), tt.yes, tt.wantErr)
			}
		})
	}
}

// TestOnlyStatuses: init --only's exit code judges only the listed steps (plus platform and
// statedir), so steps it didn't apply, like softnet without sudo, don't fail it.
func TestOnlyStatuses(t *testing.T) {
	sts := []host.Status{
		{Step: "platform", State: host.StateOK}, {Step: "statedir", State: host.StateOK},
		{Step: "softnet", State: host.StateMissing}, {Step: "basevm", State: host.StateOK},
	}
	for _, tt := range []struct {
		only []string
		ok   bool
	}{
		{nil, false},
		{[]string{"basevm"}, true},
		{[]string{"basevm", "softnet"}, false},
	} {
		if got := host.AllOK(onlyStatuses(sts, tt.only)); got != tt.ok {
			t.Errorf("--only %v: AllOK = %v, want %v", tt.only, got, tt.ok)
		}
	}
	sts[0].State = host.StateWrong
	if host.AllOK(onlyStatuses(sts, []string{"basevm"})) {
		t.Error("--only basevm with a failed platform check: AllOK, want not")
	}
}

// linuxRunner answers like an Ubuntu amd64 host: df and ip route. Any other command fails the
// test.
type linuxRunner struct{ t *testing.T }

func (r linuxRunner) Run(_ context.Context, out io.Writer, name string, args ...string) error {
	answers := map[string]string{
		"df": "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/nvme0n1p2 982797652 1 737000000 25% /",
		"ip": "default via 192.168.1.1 dev wlp0s20f3 proto dhcp metric 600\n172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1",
	}
	a, ok := answers[name]
	if !ok {
		r.t.Errorf("unexpected command %s %v", name, args)
		return errors.New("unexpected")
	}
	_, _ = io.WriteString(out, a+"\n")
	return nil
}

// kataHostDeps is a Linux host whose kata platform step passes: its /dev, /proc, /sys and
// /run under a temp Root (/dev/kvm a link to /dev/null, a character device), the tools on
// PATH, and no XDG_STATE_HOME.
func kataHostDeps(t *testing.T) hostDeps {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the kata host's fixtures need /dev/null")
	}
	home, root := t.TempDir(), t.TempDir()
	for p, body := range map[string]string{
		"proc/cpuinfo":                     "processor\t: 0\nflags\t\t: fpu vme vmx sse4_2\n",
		"proc/meminfo":                     "MemTotal:       64395636 kB\n",
		"sys/fs/cgroup/cgroup.controllers": "cpu io memory pids\n",
		"run/systemd/system/.keep":         "",
	} {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "dev", "kvm")); err != nil {
		t.Fatal(err)
	}
	return hostDeps{
		Config: config.Options{Home: func() (string, error) { return home, nil }, GOOS: "linux"},
		Env: host.Env{
			Runner: linuxRunner{t}, Fetcher: noFetch{t}, Root: root, Home: home, GOOS: "linux", GOARCH: "amd64",
			LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil },
			Getenv:   func(string) string { return "" },
			Prompt:   func(string) (bool, error) { t.Error("unexpected prompt"); return false, nil },
		},
	}
}

// TestHostKata: on a Linux host whose platform passes, check --driver kata reports steps 2 to 7
// missing; init --dry-run prints the plan and the sudo script; init --only statedir makes the
// state dir and exits 0. The config's default driver on Linux is kata too.
func TestHostKata(t *testing.T) {
	d := kataHostDeps(t)
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := runHost(context.Background(), args, &out, &errOut, d)
		return code, out.String() + errOut.String()
	}
	state := filepath.Join(d.Env.Home, ".local", "state", "factory-agent")
	code, out := run("check", "--driver", "kata")
	if code != 1 || !strings.Contains(out, "platform  ok       linux/amd64, KVM, cgroup v2, 61 GiB memory, 702 GiB free on /var/lib\n") ||
		!strings.Contains(out, "statedir  missing  "+state+"\n") {
		t.Fatalf("check before init: code %d\n%s", code, out)
	}
	_, out = run("check", "--driver", "kata", "--json")
	var sts []host.Status
	if err := json.Unmarshal([]byte(out), &sts); err != nil || len(sts) != 7 {
		t.Fatalf("check --json: %v\n%s", err, out)
	}
	for _, st := range sts[1:] {
		if st.State != host.StateMissing {
			t.Errorf("check: %+v, want missing", st)
		}
	}
	if code, out := run("init", "--dry-run"); code != 0 || !strings.Contains(out, "plan:\n  statedir  create "+state+" with mode 0700\n") ||
		!strings.Contains(out, "sudo script (one sudo prompt):\n#!/bin/sh\n") || !strings.Contains(out, state+"/stage/nerdctl-full.tar.gz") {
		t.Fatalf("init --dry-run: code %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("init --dry-run made the state dir")
	}
	if code, out := run("init", "--driver", "kata", "--only", "statedir"); code != 0 {
		t.Fatalf("init --only statedir: code %d\n%s", code, out)
	}
	if code, out := run("check", "--driver", "kata", "--json"); code != 1 || !strings.Contains(out, `{"step":"statedir","state":"ok","detail":"`+state+`"}`) {
		t.Fatalf("check after init: code %d\n%s", code, out)
	}
}

// TestHostKataTartFlags: the tart driver's flags are usage errors with kata, from --driver or
// from the config's default.
func TestHostKataTartFlags(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--driver", "kata", "--no-pf"},
		{"init", "--driver", "kata", "--remove-pf"},
		{"init", "--driver", "kata", "--remove-pf", "--dry-run"},
		{"check", "--driver", "kata", "--agent-linux", "/x/factory-agent-linux-arm64"},
		{"init", "--agent-linux", "/x/factory-agent-linux-arm64"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d := kataHostDeps(t)
			var out, errOut bytes.Buffer
			if code := runHost(context.Background(), args, &out, &errOut, d); code != 2 || !strings.Contains(errOut.String(), "is the tart driver's, not kata's") {
				t.Errorf("code %d, want 2; stderr %q", code, errOut.String())
			}
			if _, err := os.Stat(filepath.Join(d.Env.Home, ".local")); err == nil {
				t.Error("a usage error made the state dir")
			}
		})
	}
}
