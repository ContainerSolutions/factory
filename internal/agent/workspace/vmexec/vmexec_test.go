package vmexec

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// tart's tests run every operation through vmexec; these check what a second driver relies
// on: its own prefix on every command, and Exec's status hook.

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	exit  error // what an Exec'd command exits with
}

func (f *fakeRunner) record(c Cmd) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
}

func (f *fakeRunner) Run(_ context.Context, c Cmd) error {
	f.record(c)
	if c.Stdin != nil {
		_, _ = io.Copy(io.Discard, c.Stdin)
	}
	switch c.Args[len(c.Args)-1] {
	case "default":
		_, _ = io.WriteString(c.Stdout, "default via 10.213.1.1 dev eth0\n")
	case "10.213.1.1":
		_, _ = io.WriteString(c.Stdout, "10.213.1.1 dev eth0 src 10.213.1.2\n")
	}
	return nil
}

func (f *fakeRunner) Start(c Cmd) (host.Process, error) {
	f.record(c)
	return &proc{err: f.exit}, nil
}

type proc struct{ err error }

func (p *proc) Exited() <-chan struct{} { c := make(chan struct{}); close(c); return c }
func (p *proc) Err() error              { return p.err }
func (p *proc) Stop()                   {}

type exitCode int

func (e exitCode) Error() string { return "exit status" }
func (e exitCode) ExitCode() int { return int(e) }

func newRemote(f *fakeRunner) *Remote {
	return New(Config{Runner: f, Label: "kata-helper exec", Grace: time.Millisecond, Prefix: func(name string, stdin bool) []string {
		p := []string{"sudo", "-n", "/helper", "exec"}
		if stdin {
			p = append(p, "-i")
		}
		return append(p, name)
	}})
}

func TestPrefix(t *testing.T) {
	f := &fakeRunner{}
	r := newRemote(f)
	ctx := context.Background()
	if nw, err := r.Network(ctx, "ws-tsk_a"); err != nil || nw.VM.String() != "10.213.1.2" || nw.Gateway.String() != "10.213.1.1" {
		t.Fatalf("network %+v, %v", nw, err)
	}
	if err := r.Put(ctx, "ws-tsk_a", t.TempDir(), "/var/lib/factory/bin"); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(ctx, "ws-tsk_a", []string{"true"}, nil, io.Discard, io.Discard, func(err error) error { return err }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sudo -n /helper exec ws-tsk_a ip -4 route show default",
		"sudo -n /helper exec ws-tsk_a ip -4 route get 10.213.1.1",
		"sudo -n /helper exec -i ws-tsk_a sudo -n sh -c " + PutScript + " /var/lib/factory/bin",
		"sudo -n /helper exec ws-tsk_a sh -c " + ExecScript + " /tmp/factory-exec-1.pid true",
	}
	if got := strings.Join(f.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestExecStatus(t *testing.T) {
	f := &fakeRunner{exit: exitCode(125)}
	var got error
	err := newRemote(f).Exec(context.Background(), "ws-tsk_a", []string{"true"}, nil, io.Discard, io.Discard, func(err error) error {
		got = err
		return errors.New("mapped")
	})
	if err == nil || err.Error() != "mapped" || !errors.Is(got, exitCode(125)) {
		t.Errorf("err %v, status got %v", err, got)
	}
	// no status: ExitError under the label.
	f.exit = errors.New("signal: killed")
	err = newRemote(f).Exec(context.Background(), "ws-tsk_a", []string{"true"}, nil, io.Discard, io.Discard, nil)
	if err == nil || err.Error() != "ws-tsk_a: kata-helper exec: signal: killed" {
		t.Errorf("err %v", err)
	}
	f.exit = exitCode(3)
	err = newRemote(f).Exec(context.Background(), "ws-tsk_a", []string{"true"}, nil, io.Discard, io.Discard, nil)
	if ee := new(workspace.ExitError); !errors.As(err, &ee) || ee.Code != 3 {
		t.Errorf("err %v, want an ExitError 3", err)
	}
}
