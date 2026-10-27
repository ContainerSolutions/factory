package katahelper

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// fakeNerdctl records each command the helper runs (its environment and argv) and answers
// with out and err.
type fakeNerdctl struct {
	calls     []string
	out, errs string
	err       error
}

func (f *fakeNerdctl) Run(_ context.Context, env []string, _ io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, fmt.Sprintf("env %s\n%s %s", strings.Join(env, " "), name, strings.Join(args, " ")))
	_, _ = io.WriteString(stdout, f.out)
	_, _ = io.WriteString(stderr, f.errs)
	return f.err
}

func run(t *testing.T, r Runner, args ...string) (int, string, string) {
	t.Helper()
	return runIn(t, Options{Runner: r}, nil, args...)
}

// runIn runs a verb with o, stdin and fresh stdout and stderr.
func runIn(t *testing.T, o Options, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	return runCtx(t, context.Background(), o, stdin, args...)
}

// runCtx is runIn with ctx: a signal ends Main's.
func runCtx(t *testing.T, ctx context.Context, o Options, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	o.Stdin, o.Stdout, o.Stderr = stdin, &out, &errOut
	code := Run(ctx, o, args)
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"help"}, {"create"}, {"protocol", "x"}, {"list", "--all"}, {"-h"},
		{"create", ws, "2", "4096", "60", "warm"}, {"start"}, {"rm", ws, ws}, {"inspect"}, {"stop", "-t", "1", ws}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeNerdctl{}
			code, out, errOut := run(t, f, args...)
			if code != 2 || out != "" || errOut != Usage+"\n" || len(f.calls) > 0 {
				t.Errorf("code %d, stdout %q, stderr %q, calls %v", code, out, errOut, f.calls)
			}
		})
	}
}

func TestProtocol(t *testing.T) {
	f := &fakeNerdctl{}
	if code, out, errOut := run(t, f, "protocol"); code != 0 || out != fmt.Sprintln(Protocol) || errOut != "" || len(f.calls) > 0 {
		t.Errorf("code %d, stdout %q, stderr %q, calls %v", code, out, errOut, f.calls)
	}
}

// TestList: the exact nerdctl command (golden), and only workspace names printed.
func TestList(t *testing.T) {
	f := &fakeNerdctl{out: "ws-tsk_01JABC\nbuildkit_buildkit\nws-1\n\n  ws-tsk_two  \nws-tsk_../x\nws-tsk_" + strings.Repeat("a", 65) + "\n"}
	code, out, errOut := run(t, f, "list")
	if code != 0 || out != "ws-tsk_01JABC\nws-tsk_two\n" || errOut != "" {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	golden(t, "list.golden", strings.Join(f.calls, "\n")+"\n")
}

func TestListFails(t *testing.T) {
	f := &fakeNerdctl{
		errs: "time=x level=warning msg=\"a warning\"\ntime=y level=fatal msg=\"cannot access containerd socket \\\"/run/factory-agent/containerd/containerd.sock\\\"\"\n",
		err:  errors.New("exit status 1"),
	}
	code, out, errOut := run(t, f, "list")
	want := "kata-helper list: nerdctl ps: exit status 1: time=y level=fatal msg=\"cannot access containerd socket \\\"/run/factory-agent/containerd/containerd.sock\\\"\"\n"
	if code != 1 || out != "" || errOut != want {
		t.Errorf("code %d, stdout %q, stderr %q, want %q", code, out, errOut, want)
	}
	// without nerdctl's stderr, the error alone.
	f = &fakeNerdctl{err: errors.New("fork/exec " + Nerdctl + ": no such file or directory")}
	if _, _, errOut := run(t, f, "list"); errOut != "kata-helper list: nerdctl ps: fork/exec "+Nerdctl+": no such file or directory\n" {
		t.Errorf("stderr %q", errOut)
	}
	// one line, whatever nerdctl printed.
	f = &fakeNerdctl{errs: "a\r\x1b[31mb", err: errors.New("exit status 1")}
	if _, _, errOut := run(t, f, "list"); strings.Count(errOut, "\n") != 1 || strings.ContainsAny(errOut, "\r\x1b") {
		t.Errorf("stderr %q is not one line", errOut)
	}
}

// TestExecRunner: a command gets exactly the helper's environment, none of the caller's.
func TestExecRunner(t *testing.T) {
	env, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env")
	}
	t.Setenv("FACTORY_SECRET", "x")
	var out bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), Env, nil, &out, io.Discard, env); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	slices.Sort(got)
	want := slices.Sorted(slices.Values(Env))
	if !slices.Equal(got, want) {
		t.Errorf("environment %q, want %q", got, want)
	}
	if err := (ExecRunner{}).Run(context.Background(), Env, nil, io.Discard, io.Discard, "/nonexistent/nerdctl"); err == nil {
		t.Error("a missing program ran")
	}
}

// TestPaths: every path the helper uses is absolute and fixed, and PATH holds no relative or
// user-writable entry.
func TestPaths(t *testing.T) {
	for _, p := range []string{HelperPath, Nerdctl, CNIPluginDir, CNIConfDir, NerdctlDataRoot, ContainerdSocket, BuildkitSocket, KataDir,
		DisksDir, SlotsDir, WarmDisk, GuestDisk, CNIStateDir, LockFile, IP, NFT, Sysctl, CP, Truncate, Pkill} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			t.Errorf("%s is not a clean absolute path", p)
		}
	}
	for d := range strings.SplitSeq(Path, ":") {
		if !filepath.IsAbs(d) || strings.HasPrefix(d, "/home") || strings.HasPrefix(d, "/tmp") {
			t.Errorf("PATH entry %q", d)
		}
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -update to rewrite):\n%s", p, got)
	}
}
