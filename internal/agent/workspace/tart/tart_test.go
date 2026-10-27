package tart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/workspace"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

const ws = "ws-tsk_0123456789abcdef0123"

func newDriver(f *fakeTart) *Driver {
	return New(Config{Bin: "tart", Path: "/usr/bin:/bin", Runner: f, Poll: time.Millisecond, Boot: 200 * time.Millisecond, Grace: 50 * time.Millisecond})
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -update to rewrite):\n%s", p, got)
	}
}

// tgz is a .tar.gz of the entries.
func tgz(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		body := h.Linkname
		if h.Typeflag == tar.TypeReg {
			h.Linkname, h.Size = "", int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = io.WriteString(tw, body)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// file is a regular tar entry; its body rides in Linkname until tgz writes it.
func file(name, body string) *tar.Header {
	return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Linkname: body}
}

// TestLifecycle runs a workspace from Create to Destroy and checks every tart call.
func TestLifecycle(t *testing.T) {
	f := newFake()
	f.bootFails = 2
	f.get = tgz(t, &tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}, file("./outcome.json", `{"status":"succeeded"}`))
	f.exec = func(c Cmd, p *fakeProc) {
		_, _ = io.WriteString(c.Stdout, `{"type":"start"}`+"\n")
		_, _ = io.WriteString(c.Stderr, "log line\n")
		p.exit(nil)
	}
	d := newDriver(f)
	ctx := context.Background()

	if err := d.Create(ctx, ws, workspace.Spec{}); err != nil {
		t.Fatal(err)
	}
	nw, err := d.Start(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := (workspace.Network{VM: netip.MustParseAddr("192.168.96.34"), Gateway: netip.MustParseAddr("192.168.96.33")}); nw != want {
		t.Errorf("network %+v, want %+v", nw, want)
	}

	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "bundle.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "skills", "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "skills", "p", "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, ws, bundle, "/var/lib/factory/run"); err != nil {
		t.Fatal(err)
	}
	wantPut := []string{`bundle.json 600 uid=0 "{}"`, `skills/ 700 uid=0 ""`, `skills/p/ 700 uid=0 ""`, `skills/p/run.sh 700 uid=0 "#!/bin/sh\n"`}
	if strings.Join(f.put, "\n") != strings.Join(wantPut, "\n") {
		t.Errorf("put:\n%s\nwant:\n%s", strings.Join(f.put, "\n"), strings.Join(wantPut, "\n"))
	}

	var stdout, stderr bytes.Buffer
	if err := d.Exec(ctx, ws, []string{"factory-agent", "run", "--bundle", "/var/lib/factory/run"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != `{"type":"start"}`+"\n" || stderr.String() != "log line\n" {
		t.Errorf("exec stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	out := filepath.Join(t.TempDir(), "out")
	if err := d.Get(ctx, ws, "/work/out", out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "outcome.json")); err != nil || string(b) != `{"status":"succeeded"}` {
		t.Errorf("got outcome.json %q, %v", b, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 1 {
		t.Errorf("Get left %d entries next to out/, want 1 (its temp file removed)", len(ents))
	}

	names, err := d.List(ctx)
	if err != nil || len(names) != 1 || names[0] != ws {
		t.Errorf("List = %v, %v; want only %s (not Phase 0's ws-1, ws-gw)", names, err, ws)
	}
	if err := d.Destroy(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.vms[ws]; ok {
		t.Error("Destroy left the VM")
	}
	golden(t, "lifecycle.golden", strings.Join(f.calls, "\n")+"\n")
}

// TestNames: no call accepts a name that isn't a workspace's, so the owner's VMs are safe.
func TestNames(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"ws-gw", "ws-1", "factory-agent-warm", "ws-tsk_a/../b", "ws-tsk_", "ws-tsk_a b", ""} {
		f := newFake()
		d := newDriver(f)
		errs := []error{
			d.Create(ctx, name, workspace.Spec{}),
			func() error { _, err := d.Start(ctx, name); return err }(),
			d.Put(ctx, name, t.TempDir(), "/var/lib/factory/run"),
			d.Exec(ctx, name, []string{"true"}, nil, io.Discard, io.Discard),
			d.Get(ctx, name, "/work/out", filepath.Join(t.TempDir(), "out")),
			d.Stop(ctx, name),
			d.Destroy(ctx, name),
		}
		for i, err := range errs {
			if err == nil {
				t.Errorf("%q: call %d accepted it", name, i)
			}
		}
		if len(f.calls) != 0 {
			t.Errorf("%q: tart ran: %v", name, f.calls)
		}
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	t.Run("size", func(t *testing.T) {
		f := newFake()
		if err := newDriver(f).Create(ctx, ws, workspace.Spec{CPU: 2, MemoryMiB: 4096}); err != nil {
			t.Fatal(err)
		}
		if got := f.calls[1]; got != "tart set "+ws+" --cpu 2 --memory 4096" {
			t.Errorf("set call %q", got)
		}
	})
	t.Run("set fails: the clone is deleted", func(t *testing.T) {
		f := newFake()
		f.fail, f.failErr = "tart set", "boom"
		err := newDriver(f).Create(ctx, ws, workspace.Spec{})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err %v", err)
		}
		if _, ok := f.vms[ws]; ok {
			t.Error("the clone is left")
		}
	})
	t.Run("negative size", func(t *testing.T) {
		f := newFake()
		if err := newDriver(f).Create(ctx, ws, workspace.Spec{CPU: -1}); err == nil || len(f.calls) != 0 {
			t.Errorf("err %v, calls %v", err, f.calls)
		}
	})
}

func TestStartFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeTart)
		want  string
	}{
		{"tart run fails", func(f *fakeTart) { f.fail, f.failErr = "tart run", "no softnet" }, "no softnet"},
		{"tart run exits", func(f *fakeTart) { f.runExits = true }, "tart run exited before its guest agent answered"},
		{"boot timeout", func(f *fakeTart) { f.neverBoot = true }, "timed out"},
		{"no default route", func(f *fakeTart) { f.routes = "" }, "want one default route"},
		{"two default routes", func(f *fakeTart) {
			f.routes = "default via 192.168.96.33 dev enp0s1\ndefault via 10.0.0.1 dev enp0s2\n"
		}, "want one default route"},
		{"ipv6 gateway", func(f *fakeTart) { f.routes = "default via fe80::1 dev enp0s1\n" }, "isn't a unicast IPv4"},
		{"garbage gateway", func(f *fakeTart) { f.routes = "default dev enp0s1\n" }, "default route"},
		{"no src", func(f *fakeTart) { f.routeGet = "192.168.96.33 dev enp0s1\n" }, "route to 192.168.96.33"},
		{"src is the gateway", func(f *fakeTart) { f.routeGet = "192.168.96.33 dev enp0s1 src 192.168.96.33\n" }, "its address is its gateway's"},
		{"loopback src", func(f *fakeTart) { f.routeGet = "192.168.96.33 dev lo src 127.0.0.1\n" }, "isn't a unicast IPv4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			if err := newDriver(f).Create(context.Background(), ws, workspace.Spec{}); err != nil {
				t.Fatal(err)
			}
			tt.setup(f)
			d := newDriver(f)
			_, err := d.Start(context.Background(), ws)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			if f.vms[ws] == "running" {
				t.Error("tart run is left running")
			}
			if len(d.procs) != 0 {
				t.Error("the driver still holds the tart run")
			}
		})
	}
}

func TestStartTwice(t *testing.T) {
	f := newFake()
	d := newDriver(f)
	ctx := context.Background()
	if err := d.Create(ctx, ws, workspace.Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, ws); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("err %v", err)
	}
}

func started(t *testing.T, f *fakeTart) *Driver {
	t.Helper()
	d := newDriver(f)
	if err := d.Create(context.Background(), ws, workspace.Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	return d
}

func TestExec(t *testing.T) {
	t.Run("exit code", func(t *testing.T) {
		f := newFake()
		f.exec = func(_ Cmd, p *fakeProc) { p.exit(fakeExit(3)) }
		err := started(t, f).Exec(context.Background(), ws, []string{"factory-agent", "run"}, nil, io.Discard, io.Discard)
		var ee *workspace.ExitError
		if !errors.As(err, &ee) || ee.Code != 3 {
			t.Fatalf("err %v, want an ExitError 3", err)
		}
	})
	t.Run("stdin", func(t *testing.T) {
		f := newFake()
		f.exec = func(c Cmd, p *fakeProc) { _, _ = io.Copy(c.Stdout, c.Stdin); p.exit(nil) }
		var out bytes.Buffer
		if err := started(t, f).Exec(context.Background(), ws, []string{"cat"}, strings.NewReader("hi"), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if out.String() != "hi" || !strings.HasPrefix(f.calls[0], "tart exec -i "+ws+" sh -c ") {
			t.Errorf("out %q, call %q", out.String(), f.calls[0])
		}
	})
	t.Run("no command", func(t *testing.T) {
		if err := started(t, newFake()).Exec(context.Background(), ws, nil, nil, io.Discard, io.Discard); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("cancel: SIGTERM, and the process exits", func(t *testing.T) {
		f := newFake()
		f.sig = make(chan string, 4)
		f.exec = func(_ Cmd, p *fakeProc) {
			if <-f.sig == "TERM" {
				p.exit(nil)
			}
		}
		d := started(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- d.Exec(ctx, ws, []string{"factory-agent", "run"}, nil, io.Discard, io.Discard) }()
		time.Sleep(10 * time.Millisecond)
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want context.Canceled", err)
		}
		if strings.Join(f.signals, ",") != "TERM /tmp/factory-exec-1.pid" || f.lastExec.stopped {
			t.Errorf("signals %v, local tart exec stopped %v", f.signals, f.lastExec.stopped)
		}
	})
	t.Run("cancel: ignored SIGTERM, then SIGKILL after the grace", func(t *testing.T) {
		f := newFake()
		f.sig = make(chan string, 4)
		f.exec = func(Cmd, *fakeProc) {}
		d := started(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := d.Exec(ctx, ws, []string{"sleep", "inf"}, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if strings.Join(f.signals, ",") != "TERM /tmp/factory-exec-1.pid,KILL /tmp/factory-exec-1.pid" {
			t.Errorf("signals %v", f.signals)
		}
		if !f.lastExec.stopped {
			t.Error("the local tart exec wasn't stopped")
		}
	})
}

func TestPutRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("a symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks")
		}
		src := t.TempDir()
		if err := os.Symlink("/etc/passwd", filepath.Join(src, "x")); err != nil {
			t.Fatal(err)
		}
		if err := started(t, newFake()).Put(ctx, ws, src, "/var/lib/factory/run"); err == nil || !strings.Contains(err.Error(), "only directories and regular files") {
			t.Fatalf("err %v", err)
		}
	})
	for _, dst := range []string{"/", "var/lib/factory/run", "/var/lib/factory/../etc", "/var/lib/factory/run/", ""} {
		t.Run("dst "+dst, func(t *testing.T) {
			f := newFake()
			if err := started(t, f).Put(ctx, ws, t.TempDir(), dst); err == nil || len(f.calls) != 0 {
				t.Fatalf("err %v, calls %v", err, f.calls)
			}
		})
	}
	t.Run("the VM's tar fails", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		f.fail, f.failErr = "sudo -n sh", "tar: write error"
		if err := d.Put(ctx, ws, t.TempDir(), "/var/lib/factory/run"); err == nil || !strings.Contains(err.Error(), "tar: write error") {
			t.Fatalf("err %v", err)
		}
	})
}

// TestGetHostile: what the VM sends back is untrusted.
func TestGetHostile(t *testing.T) {
	tests := []struct {
		name string
		get  func(t *testing.T) []byte
		want string
	}{
		{"traversal", func(t *testing.T) []byte { return tgz(t, file("../evil", "x")) }, "outside the archive"},
		{"absolute", func(t *testing.T) []byte { return tgz(t, file("/etc/evil", "x")) }, "outside the archive"},
		{"symlink out", func(t *testing.T) []byte {
			return tgz(t, &tar.Header{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "../../etc"})
		}, "points outside"},
		{"fifo", func(t *testing.T) []byte { return tgz(t, &tar.Header{Name: "./p", Typeflag: tar.TypeFifo}) }, "unsupported type"},
		{"not a tarball", func(t *testing.T) []byte { return []byte("this is not a gzip stream") }, "gzip"},
		{"over the cap", func(t *testing.T) []byte { return bytes.Repeat([]byte("x"), 2048) }, "more than 1024 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			d := started(t, f)
			d.c.MaxGet = 1024
			f.get = tt.get(t)
			out := filepath.Join(t.TempDir(), "out")
			err := d.Get(context.Background(), ws, "/work/out", out)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 0 {
				t.Errorf("Get left %d entries", len(ents))
			}
		})
	}
	t.Run("dst exists", func(t *testing.T) {
		f := newFake()
		if err := started(t, f).Get(context.Background(), ws, "/work/out", t.TempDir()); err == nil || len(f.calls) != 0 {
			t.Fatalf("err %v, calls %v", err, f.calls)
		}
	})
}

// TestGetFiles: Get with names tars only those, a missing one a warning (GNU tar's
// --ignore-failed-read), and refuses a name that isn't a plain one before tart runs.
func TestGetFiles(t *testing.T) {
	f := newFake()
	d := started(t, f)
	f.get = tgz(t, file("outcome.json", `{"status":"succeeded"}`))
	out := filepath.Join(t.TempDir(), "out")
	if err := d.Get(context.Background(), ws, "/work/out", out, "outcome.json", "transcript.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	want := "tart exec " + ws + " sudo -n tar -czf - --ignore-failed-read -C /work/out outcome.json transcript.jsonl.gz"
	if len(f.calls) != 1 || f.calls[0] != want {
		t.Errorf("calls %q, want %q", f.calls, want)
	}
	if b, err := os.ReadFile(filepath.Join(out, "outcome.json")); err != nil || string(b) != `{"status":"succeeded"}` {
		t.Errorf("got outcome.json %q, %v", b, err)
	}
	for _, name := range []string{"", ".", "..", "../x", "a/b", "-C", "--to-command=sh"} {
		f.calls = nil
		dst := filepath.Join(t.TempDir(), "out")
		if err := d.Get(context.Background(), ws, "/work/out", dst, "outcome.json", name); err == nil || len(f.calls) != 0 {
			t.Errorf("%q: err %v, calls %v", name, err, f.calls)
		}
		if _, err := os.Lstat(dst); err == nil {
			t.Errorf("%q: Get made dst", name)
		}
	}
}

func TestStopAndDestroy(t *testing.T) {
	ctx := context.Background()
	t.Run("stop keeps the VM", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		if err := d.Stop(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if f.vms[ws] != "stopped" || len(d.procs) != 0 {
			t.Errorf("state %q, procs %d", f.vms[ws], len(d.procs))
		}
		// a second stop meets "is not running": still fine.
		if err := d.Stop(ctx, ws); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("destroy a stopped VM", func(t *testing.T) {
		f := newFake()
		d := newDriver(f)
		if err := d.Create(ctx, ws, workspace.Spec{}); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		if err := d.Destroy(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(f.calls, "; "); got != "tart list --format json; tart delete "+ws {
			t.Errorf("calls %s", got)
		}
	})
	t.Run("destroy a missing VM", func(t *testing.T) {
		f := newFake()
		if err := newDriver(f).Destroy(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 {
			t.Errorf("calls %v", f.calls)
		}
	})
	t.Run("destroy after a cancel still stops, deletes and reaps", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := d.Destroy(cctx, ws); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(f.calls, "; "); got != "tart list --format json; tart stop "+ws+"; tart delete "+ws {
			t.Errorf("calls %s", got)
		}
		if _, ok := f.vms[ws]; ok || len(d.procs) != 0 || len(f.runs) != 0 {
			t.Errorf("vm %q, procs %d, runs %d", f.vms[ws], len(d.procs), len(f.runs))
		}
	})
	t.Run("stop after a cancel still stops", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := d.Stop(cctx, ws); err != nil {
			t.Fatal(err)
		}
		if f.vms[ws] != "stopped" || len(d.procs) != 0 || len(f.runs) != 0 {
			t.Errorf("state %q, procs %d, runs %d", f.vms[ws], len(d.procs), len(f.runs))
		}
	})
	t.Run("destroy: list fails, tart run is still reaped", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		run := f.runs[ws]
		f.fail, f.failErr = "tart list", "boom"
		if err := d.Destroy(ctx, ws); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err %v", err)
		}
		if len(d.procs) != 0 || len(f.runs) != 0 || !run.stopped || f.vms[ws] != "stopped" {
			t.Errorf("procs %d, runs %d, tart run SIGTERMed %v, state %q", len(d.procs), len(f.runs), run.stopped, f.vms[ws])
		}
	})
	t.Run("destroy: delete fails", func(t *testing.T) {
		f := newFake()
		d := started(t, f)
		f.fail, f.failErr = "tart delete", "disk busy"
		if err := d.Destroy(ctx, ws); err == nil || !strings.Contains(err.Error(), "disk busy") {
			t.Fatalf("err %v", err)
		}
	})
}

// TestExecRunner runs sh (never tart): the env, stdin and the separate stdout and stderr.
func TestExecRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	var out, errb bytes.Buffer
	err := ExecRunner{}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", `echo "$FOO"; cat; echo e >&2; exit 3`},
		Env: []string{"FOO=bar"}, Stdin: strings.NewReader("in\n"), Stdout: &out, Stderr: &errb,
	})
	var ec interface{ ExitCode() int }
	if !errors.As(err, &ec) || ec.ExitCode() != 3 {
		t.Fatalf("err %v", err)
	}
	if out.String() != "bar\nin\n" || errb.String() != "e\n" {
		t.Errorf("out %q, err %q", out.String(), errb.String())
	}
	p, err := ExecRunner{}.Start(Cmd{Name: "sh", Args: []string{"-c", "exit 4"}})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Exited()
	if err := exitError(ws, p.Err()); !errors.As(err, new(*workspace.ExitError)) {
		t.Errorf("exitError %v", err)
	}
}
