package kata

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
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/workspace"
	"github.com/ContainerSolutions/factory/internal/agent/workspace/vmexec"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

const (
	ws    = "ws-tsk_0123456789abcdef0123"
	image = "factory-agent-base:0.1.0"
)

func newDriver(t *testing.T, f *fakeHelper) *Driver {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "factory-agent")
	if err := os.WriteFile(bin, []byte("#!agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(Config{Runner: f, Image: image, DiskGiB: 60, Warm: true, AgentBinary: bin, Poll: time.Millisecond, Boot: 200 * time.Millisecond, Grace: 50 * time.Millisecond})
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

// tgz is a .tar.gz of the regular files name -> body.
func tgz(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, f[1])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestLifecycle runs a workspace from Create to Destroy and checks every helper call.
func TestLifecycle(t *testing.T) {
	f := newFake()
	f.bootFails = 2
	f.get = tgz(t, [2]string{"./outcome.json", `{"status":"succeeded"}`})
	f.exec = func(c vmexec.Cmd, p *fakeProc) {
		_, _ = io.WriteString(c.Stdout, `{"type":"start"}`+"\n")
		_, _ = io.WriteString(c.Stderr, "log line\n")
		p.exit(nil)
	}
	d := newDriver(t, f)
	ctx := context.Background()
	if err := d.Create(ctx, ws, workspace.Spec{}); err != nil {
		t.Fatal(err)
	}
	nw, err := d.Start(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := (workspace.Network{VM: netip.MustParseAddr("10.213.1.2"), Gateway: netip.MustParseAddr("10.213.1.1")}); nw != want {
		t.Errorf("network %+v, want %+v", nw, want)
	}
	if got := strings.Join(f.put, "\n"); got != `factory-agent 755 "#!agent"` {
		t.Errorf("the agent binary's put: %s", got)
	}
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "bundle.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, ws, bundle, "/var/lib/factory/run"); err != nil {
		t.Fatal(err)
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
	if err := d.Stop(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if names, err := d.List(ctx); err != nil || len(names) != 1 || names[0] != ws {
		t.Errorf("List = %v, %v; want only %s (not ws-gw)", names, err, ws)
	}
	if err := d.Destroy(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.ws[ws]; ok {
		t.Error("Destroy left the workspace")
	}
	golden(t, "lifecycle.golden", strings.Join(f.calls, "\n")+"\n")
}

// TestNames: no call accepts a name that isn't a workspace's, and the helper never runs.
func TestNames(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"ws-gw", "ws-1", "ws-tsk_a/../b", "ws-tsk_", "-i", ""} {
		f := newFake()
		d := newDriver(t, f)
		_, startErr := d.Start(ctx, name)
		for i, err := range []error{
			d.Create(ctx, name, workspace.Spec{}), startErr,
			d.Put(ctx, name, t.TempDir(), "/var/lib/factory/run"),
			d.Exec(ctx, name, []string{"true"}, nil, io.Discard, io.Discard),
			d.Get(ctx, name, "/work/out", filepath.Join(t.TempDir(), "out")),
			d.Stop(ctx, name), d.Destroy(ctx, name),
		} {
			if err == nil {
				t.Errorf("%q: call %d accepted it", name, i)
			}
		}
		if len(f.calls) != 0 {
			t.Errorf("%q: the helper ran: %v", name, f.calls)
		}
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		spec workspace.Spec
		warm bool
		want string
	}{
		{workspace.Spec{CPU: 4, MemoryMiB: 4096}, true, " 3 4096 60 warm " + image},
		{workspace.Spec{CPU: 1, MemoryMiB: 2048}, false, " 1 2048 60 empty " + image},
	} {
		f := newFake()
		d := newDriver(t, f)
		d.c.Warm = tt.warm
		if err := d.Create(ctx, ws, tt.spec); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 || !strings.HasSuffix(f.calls[0], "create "+ws+tt.want) {
			t.Errorf("calls %q, want create …%s", f.calls, tt.want)
		}
	}
	f := newFake()
	d := newDriver(t, f)
	d.c.Image = ""
	if err := d.Create(ctx, ws, workspace.Spec{}); err == nil || len(f.calls) != 0 {
		t.Errorf("no image: err %v, calls %v", err, f.calls)
	}
	d.c.Image, d.c.DiskGiB = image, 0
	if err := d.Create(ctx, ws, workspace.Spec{}); err == nil || len(f.calls) != 0 {
		t.Errorf("no disk size: err %v, calls %v", err, f.calls)
	}
	d.c.DiskGiB = 60
	f.fail, f.failErr = "create", "no free slot: all 63 are in use"
	d.c.Image = image
	if err := d.Create(ctx, ws, workspace.Spec{}); err == nil || !strings.Contains(err.Error(), "no free slot") {
		t.Errorf("err %v", err)
	}
}

func TestStartFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeHelper, *Driver)
		want  string
	}{
		{"start fails", func(f *fakeHelper, _ *Driver) { f.fail, f.failErr = "kata-helper start", "nerdctl start: boom" }, "nerdctl start: boom"},
		{"boot timeout", func(f *fakeHelper, _ *Driver) { f.neverBoot = true }, "waiting for its systemd"},
		{"no default route", func(f *fakeHelper, _ *Driver) { f.routes = "" }, "want one default route"},
		{"src is the gateway", func(f *fakeHelper, _ *Driver) { f.routeGet = "10.213.1.1 dev eth0 src 10.213.1.1\n" }, "its address is its gateway's"},
		{"routes disagree with the slot", func(f *fakeHelper, _ *Driver) {
			f.inspect = `{"slot":2,"vm":"10.213.2.2","gateway":"10.213.2.1","running":true}`
		}, "its slot 2 is 10.213.2.2, 10.213.2.1"},
		{"inspect isn't JSON", func(f *fakeHelper, _ *Driver) { f.inspect = "nope" }, "kata-helper inspect"},
		{"no agent binary", func(_ *fakeHelper, d *Driver) { d.c.AgentBinary = "/nonexistent/factory-agent" }, "the agent binary"},
		{"the link fails", func(f *fakeHelper, _ *Driver) { f.fail, f.failErr = "sudo -n ln -sfn", "read-only" }, "linking /usr/local/bin/factory-agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			d := newDriver(t, f)
			if err := d.Create(context.Background(), ws, workspace.Spec{}); err != nil {
				t.Fatal(err)
			}
			tt.setup(f, d)
			if _, err := d.Start(context.Background(), ws); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			for _, c := range f.calls {
				if strings.Contains(c, "kata-helper rm") || strings.Contains(c, "kata-helper stop") {
					t.Errorf("Start cleaned up itself: %s", c)
				}
			}
		})
	}
}

func started(t *testing.T, f *fakeHelper) *Driver {
	t.Helper()
	d := newDriver(t, f)
	if err := d.Create(context.Background(), ws, workspace.Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	return d
}

// TestExecStatus: a non-zero status is the remote command's, but the helper's 125 with its
// kata-helper: line last, which is an exec failure.
func TestExecStatus(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stderr string
		code   int
		exit   int // the ExitError's code; 0 for an exec failure
		want   string
	}{
		{"remote status", "boom\n", 3, 3, "exit status 3"},
		{"remote 125", "its own error\n", 125, 125, "exit status 125"},
		{"helper failure", "remote noise\nkata-helper: nerdctl exec: exit status 1\n", 125, 0, "kata-helper exec: nerdctl exec: exit status 1"},
		{"helper line not last", "kata-helper: x\nmore\n", 125, 125, "exit status 125"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.exec = func(c vmexec.Cmd, p *fakeProc) { _, _ = io.WriteString(c.Stderr, tt.stderr); p.exit(fakeExit(tt.code)) }
			var stderr bytes.Buffer
			err := started(t, f).Exec(context.Background(), ws, []string{"factory-agent", "run"}, nil, io.Discard, &stderr)
			var ee *workspace.ExitError
			if err == nil || !strings.Contains(err.Error(), tt.want) || errors.As(err, &ee) != (tt.exit != 0) || (ee != nil && ee.Code != tt.exit) {
				t.Errorf("err %v, want %q (exit %d)", err, tt.want, tt.exit)
			}
			if stderr.String() != tt.stderr {
				t.Errorf("stderr %q", stderr.String())
			}
		})
	}
	t.Run("stdin", func(t *testing.T) {
		f := newFake()
		f.exec = func(c vmexec.Cmd, p *fakeProc) { _, _ = io.Copy(c.Stdout, c.Stdin); p.exit(nil) }
		var out bytes.Buffer
		if err := started(t, f).Exec(context.Background(), ws, []string{"cat"}, strings.NewReader("hi"), &out, nil); err != nil {
			t.Fatal(err)
		}
		if want := "sudo -n /usr/local/libexec/factory-agent/kata-helper exec -i " + ws + " sh -c "; out.String() != "hi" || !strings.HasPrefix(f.calls[0], want) {
			t.Errorf("out %q, call %q", out.String(), f.calls[0])
		}
	})
}

// TestCancel: SIGTERM through another kata-helper exec, then SIGKILL and the local stop.
func TestCancel(t *testing.T) {
	t.Run("TERM honoured", func(t *testing.T) {
		f := newFake()
		f.sig = make(chan string, 4)
		f.exec = func(_ vmexec.Cmd, p *fakeProc) {
			if <-f.sig == "TERM" {
				p.exit(fakeExit(143))
			}
		}
		d := started(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- d.Exec(ctx, ws, []string{"factory-agent", "run"}, nil, io.Discard, io.Discard) }()
		time.Sleep(10 * time.Millisecond)
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if strings.Join(f.signals, ",") != "TERM /tmp/factory-exec-1.pid" || f.lastExec.stopped {
			t.Errorf("signals %v, stopped %v", f.signals, f.lastExec.stopped)
		}
		if !strings.Contains(f.calls[1], "kata-helper exec "+ws+" sh -c ") {
			t.Errorf("the signal didn't go through the helper: %q", f.calls[1])
		}
	})
	t.Run("TERM ignored", func(t *testing.T) {
		f := newFake()
		f.sig = make(chan string, 4)
		f.exec = func(vmexec.Cmd, *fakeProc) {}
		d := started(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := d.Exec(ctx, ws, []string{"sleep", "inf"}, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if strings.Join(f.signals, ",") != "TERM /tmp/factory-exec-1.pid,KILL /tmp/factory-exec-1.pid" || !f.lastExec.stopped {
			t.Errorf("signals %v, stopped %v", f.signals, f.lastExec.stopped)
		}
	})
}

// TestGetHostile: what the VM sends back is untrusted.
func TestGetHostile(t *testing.T) {
	for _, tt := range []struct {
		name string
		get  func(t *testing.T) []byte
		want string
	}{
		{"traversal", func(t *testing.T) []byte { return tgz(t, [2]string{"../evil", "x"}) }, "outside the archive"},
		{"over the cap", func(*testing.T) []byte { return bytes.Repeat([]byte("x"), 2048) }, "more than 1024 bytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			d := started(t, f)
			d.c.MaxGet = 1024
			f.get = tt.get(t)
			out := filepath.Join(t.TempDir(), "out")
			if err := d.Get(context.Background(), ws, "/work/out", out); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 0 {
				t.Errorf("Get left %d entries", len(ents))
			}
		})
	}
}

func TestStopDestroyAfterCancel(t *testing.T) {
	f := newFake()
	d := started(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Stop(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "; "); !strings.Contains(got, "kata-helper stop "+ws) || !strings.Contains(got, "kata-helper rm "+ws) {
		t.Errorf("calls %s", got)
	}
	f.fail, f.failErr = "kata-helper rm", "nerdctl rm: after killing its processes: timed out"
	if err := d.Destroy(context.Background(), ws); err == nil || !strings.Contains(err.Error(), "after killing") {
		t.Errorf("err %v", err)
	}
}
