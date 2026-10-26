package katahelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const ws = "ws-tsk_0123456789abcdef0123"

// fakeHost answers the helper's commands as the host would and records them: nerdctl keeps
// containers, ip link add/del makes and removes /sys/class/net/<bridge> under root, cp and
// truncate make the disk. A call containing fail fails; one containing hang blocks until its
// context is done, hangs times.
type fakeHost struct {
	t    *testing.T
	root string

	mu         sync.Mutex
	calls      []string
	containers map[string]bool // name -> running
	fail, hang string
	hangs      int
}

func newHost(t *testing.T) *fakeHost {
	return &fakeHost{t: t, root: t.TempDir(), containers: map[string]bool{}}
}

func (f *fakeHost) opts() Options {
	return Options{Runner: f, Root: f.root, Bound: 50 * time.Millisecond, KillWait: time.Millisecond}
}

// fakeExit is an exit status, as *exec.ExitError reports it.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

func id(name string) string { s := sha256.Sum256([]byte(name)); return hex.EncodeToString(s[:]) }

func (f *fakeHost) Run(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	if !slices.Equal(env, Env) {
		f.t.Errorf("%s ran with environment %q", name, env)
	}
	call := name + " " + strings.Join(args, " ")
	if name == Nerdctl {
		if !slices.Equal(args[:len(NerdctlGlobal)], NerdctlGlobal) {
			f.t.Errorf("nerdctl without its global flags: %s", call)
		}
		args = args[len(NerdctlGlobal):]
		call = "nerdctl " + strings.Join(args, " ")
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hang := f.hang != "" && strings.Contains(call, f.hang) && f.hangs > 0
	if hang {
		f.hangs--
	}
	fail := f.fail != "" && strings.Contains(call, f.fail)
	f.mu.Unlock()
	switch {
	case hang:
		<-ctx.Done()
		return ctx.Err()
	case fail:
		_, _ = io.WriteString(stderr, "a warning\nboom\n")
		return fakeExit(1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	last := args[len(args)-1]
	switch name {
	case IP:
		p := filepath.Join(f.root, "sys/class/net", args[2])
		switch args[0] + " " + args[1] {
		case "link add":
			return os.MkdirAll(p, 0o755)
		case "link del":
			return os.Remove(p)
		}
	case CP, Truncate:
		return os.WriteFile(f.root+last, nil, 0o600)
	case Pkill:
		return fakeExit(1)
	case Nerdctl:
		switch args[0] {
		case "ps":
			for _, n := range slices.Sorted(func(yield func(string) bool) {
				for n := range f.containers {
					if !yield(n) {
						return
					}
				}
			}) {
				_, _ = fmt.Fprintf(stdout, "%s %s\n", id(n), n)
			}
		case "create":
			f.containers[args[2]] = false
		case "start":
			f.containers[last] = true
		case "stop":
			f.containers[last] = false
		case "rm":
			delete(f.containers, last)
		case "inspect":
			_, _ = fmt.Fprintln(stdout, f.containers[last])
		}
	}
	return nil
}

// files lists what's under root but the lock and the directories.
func (f *fakeHost) files() []string {
	var out []string
	_ = filepath.WalkDir(f.root, func(p string, e os.DirEntry, err error) error {
		if err == nil && (!e.IsDir() || strings.Contains(p, "/sys/class/net/")) && !strings.HasSuffix(p, "kata.lock") {
			out = append(out, strings.TrimPrefix(p, f.root))
		}
		return err
	})
	return out
}

func TestCreateArgs(t *testing.T) {
	for _, args := range [][]string{
		{"ws-gw", "2", "4096", "warm", "factory-agent-base:dev"},
		{"ws-tsk_a/../b", "2", "4096", "warm", "factory-agent-base:dev"},
		{ws, "0", "4096", "warm", "factory-agent-base:dev"},
		{ws, "17", "4096", "warm", "factory-agent-base:dev"},
		{ws, "02", "4096", "warm", "factory-agent-base:dev"},
		{ws, "+2", "4096", "warm", "factory-agent-base:dev"},
		{ws, "2", "2047", "warm", "factory-agent-base:dev"},
		{ws, "2", "65537", "warm", "factory-agent-base:dev"},
		{ws, "2", "4096", "hot", "factory-agent-base:dev"},
		{ws, "2", "4096", "warm", "ubuntu:24.04"},
		{ws, "2", "4096", "warm", "factory-agent-base:-x"},
		{ws, "2", "4096", "warm", "factory-agent-base:a b"},
		{ws, "2", "4096", "warm", "factory-agent-base:dev@sha256:00"},
	} {
		f := newHost(t)
		code, _, errOut := runIn(t, f.opts(), append([]string{"create"}, args...)...)
		if code != 2 || !strings.HasPrefix(errOut, "kata-helper create: ") || len(f.calls) > 0 || len(f.files()) > 0 {
			t.Errorf("%q: code %d, stderr %q, calls %v, files %v", args, code, errOut, f.calls, f.files())
		}
	}
	f := newHost(t)
	if code, _, _ := runIn(t, f.opts(), "rm", "ws-1"); code != 2 || len(f.calls) > 0 {
		t.Errorf("rm ws-1: code %d, calls %v", code, f.calls)
	}
	if code, _, errOut := runIn(t, f.opts(), "create", ws, "16", "65536", "empty", "factory-agent-base:v1.2.3-rc.1_x"); code != 0 {
		t.Errorf("the largest size: code %d, stderr %q", code, errOut)
	}
}

// TestLifecycle: create's and rm's exact commands (golden), create's output, files and
// conflist.
func TestLifecycle(t *testing.T) {
	f := newHost(t)
	if err := os.MkdirAll(f.root+KataStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.root+WarmDisk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runIn(t, f.opts(), "create", ws, "5", "8192", "warm", "factory-agent-base:0.1.0")
	if code != 0 || out != `{"slot":1,"vm":"10.213.1.2","gateway":"10.213.1.1"}`+"\n" {
		t.Fatalf("create: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := []string{CNIConfDir + "/ws-1.conflist", "/sys/class/net/fws1", DisksDir + "/" + ws + ".img", SlotsDir + "/1", WarmDisk}
	if got := f.files(); !slices.Equal(got, want) {
		t.Errorf("files %q, want %q", got, want)
	}
	conf, err := os.ReadFile(f.root + CNIConfDir + "/ws-1.conflist")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "conflist.golden", string(conf))
	if code, _, errOut := runIn(t, f.opts(), "rm", ws); code != 0 {
		t.Fatalf("rm: code %d, stderr %q", code, errOut)
	}
	if got := f.files(); !slices.Equal(got, []string{WarmDisk}) {
		t.Errorf("rm left %q", got)
	}
	golden(t, "lifecycle.golden", strings.Join(f.calls, "\n")+"\n")
}

// TestCreateUndo: a failure at each stage of create leaves nothing behind.
func TestCreateUndo(t *testing.T) {
	for _, stage := range []string{"ip link add", "sysctl", "ip addr add", "ip link set", "nft add element", "truncate", "nerdctl create"} {
		t.Run(stage, func(t *testing.T) {
			f := newHost(t)
			f.fail = strings.TrimPrefix(strings.TrimPrefix(stage, "ip "), "nerdctl ")
			code, out, errOut := runIn(t, f.opts(), "create", ws, "5", "8192", "empty", "factory-agent-base:dev")
			if code != 1 || out != "" || !strings.Contains(errOut, "boom") {
				t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
			}
			if got := f.files(); len(got) > 0 {
				t.Errorf("left %q; calls:\n%s", got, strings.Join(f.calls, "\n"))
			}
			if last := f.calls[len(f.calls)-1]; strings.Contains(stage, "ip link add") != strings.HasPrefix(last, NFT+" destroy") {
				t.Errorf("last call %q", last)
			}
		})
	}
	t.Run("the disk exists", func(t *testing.T) {
		f := newHost(t)
		if err := os.MkdirAll(f.root+DisksDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.root+DisksDir+"/"+ws+".img", []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runIn(t, f.opts(), "create", ws, "5", "8192", "empty", "factory-agent-base:dev"); code != 1 || !strings.Contains(errOut, "already has a disk") || len(f.calls) > 0 {
			t.Errorf("code %d, stderr %q, calls %v", code, errOut, f.calls)
		}
		if b, _ := os.ReadFile(f.root + DisksDir + "/" + ws + ".img"); string(b) != "x" {
			t.Error("the existing disk was touched")
		}
	})
	t.Run("no warm disk", func(t *testing.T) {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), "create", ws, "5", "8192", "warm", "factory-agent-base:dev"); code != 1 || !strings.Contains(errOut, "no warm disk") || len(f.calls) > 0 {
			t.Errorf("code %d, stderr %q, calls %v", code, errOut, f.calls)
		}
	})
}

// TestSlots: concurrent creates get distinct slots under the lock; a name gets one; rm frees
// its slot for the next create; the 64th workspace is refused.
func TestSlots(t *testing.T) {
	f := newHost(t)
	create := func(name string) (int, string) {
		code, out, errOut := runIn(t, f.opts(), "create", name, "1", "2048", "empty", "factory-agent-base:dev")
		return code, out + errOut
	}
	var wg sync.WaitGroup
	for i := range Slots {
		wg.Go(func() {
			if code, out := create(fmt.Sprintf("ws-tsk_%02d", i)); code != 0 {
				t.Errorf("create %d: %d %s", i, code, out)
			}
		})
	}
	wg.Wait()
	m, err := (&helper{o: f.opts()}).slots()
	if err != nil || len(m) != Slots {
		t.Fatalf("%d slots, %v", len(m), err)
	}
	if code, out := create("ws-tsk_extra"); code != 1 || !strings.Contains(out, "no free slot") {
		t.Errorf("64th: %d %s", code, out)
	}
	if code, out := create("ws-tsk_07"); code != 1 || !strings.Contains(out, "already") {
		t.Errorf("a second ws-tsk_07: %d %s", code, out)
	}
	n := 0
	for s, name := range m {
		if name == "ws-tsk_07" {
			n = s
		}
	}
	if code, out, errOut := runIn(t, f.opts(), "rm", "ws-tsk_07"); code != 0 {
		t.Fatalf("rm: %d %s %s", code, out, errOut)
	}
	if code, out := create("ws-tsk_new"); code != 0 || !strings.HasPrefix(out, fmt.Sprintf(`{"slot":%d,`, n)) {
		t.Errorf("after rm: %d %s, want slot %d", code, out, n)
	}
}

// TestRm: a missing workspace is no error; a hung rm -f kills the container's processes by id
// and retries; a failing retry keeps the slot.
func TestRm(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), "rm", ws); code != 0 || len(f.calls) != 1 {
			t.Errorf("code %d, stderr %q, calls %v", code, errOut, f.calls)
		}
	})
	created := func(t *testing.T) *fakeHost {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), "create", ws, "5", "8192", "empty", "factory-agent-base:dev"); code != 0 {
			t.Fatal(errOut)
		}
		f.calls = nil
		return f
	}
	t.Run("hung", func(t *testing.T) {
		f := created(t)
		f.hang, f.hangs = "rm -f", 1
		if code, _, errOut := runIn(t, f.opts(), "rm", ws); code != 0 {
			t.Fatalf("code %d, stderr %q", code, errOut)
		}
		want := []string{"nerdctl ps", "nerdctl stop -t 30 " + ws, "nerdctl rm -f " + ws, Pkill + " -KILL -f -- " + id(ws), "nerdctl rm -f " + ws, NFT + " destroy"}
		for i, w := range want {
			if i >= len(f.calls) || !strings.HasPrefix(f.calls[i], w) {
				t.Fatalf("calls:\n%s\nwant prefixes %q", strings.Join(f.calls, "\n"), want)
			}
		}
		if got := f.files(); len(got) > 0 {
			t.Errorf("left %q", got)
		}
	})
	t.Run("still hung", func(t *testing.T) {
		f := created(t)
		f.hang, f.hangs = "rm -f", 2
		code, _, errOut := runIn(t, f.opts(), "rm", ws)
		if code != 1 || !strings.Contains(errOut, "after killing its processes") || !strings.Contains(errOut, "timed out") {
			t.Fatalf("code %d, stderr %q", code, errOut)
		}
		if got := f.files(); !slices.Contains(got, SlotsDir+"/1") {
			t.Errorf("the slot was freed: %q", got)
		}
	})
}
