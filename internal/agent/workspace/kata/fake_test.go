package kata

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/katahelper"
	"github.com/compliance-framework/factory/internal/agent/workspace/vmexec"
)

// fakeHelper answers sudo -n kata-helper <verb> as the helper would and records each call;
// nothing runs sudo. A call containing fail fails with failErr as the helper's stderr line.
type fakeHelper struct {
	mu        sync.Mutex
	calls     []string
	ws        map[string]string // name -> created | running | stopped
	bootFails int               // test -S /run/systemd/private fails this many times first
	neverBoot bool
	routes    string // ip -4 route show default's answer
	routeGet  string // ip -4 route get's answer
	inspect   string // inspect's answer
	fail      string
	failErr   string
	put       []string // the tar entries Put sent: "name mode body"
	get       []byte   // tar -czf's answer
	signals   []string
	sig       chan string // gets each signal sent, when set (buffered)
	// exec is what an Exec'd command does.
	exec     func(c vmexec.Cmd, p *fakeProc)
	lastExec *fakeProc
}

func newFake() *fakeHelper {
	return &fakeHelper{
		ws:       map[string]string{},
		routes:   "default via 10.213.1.1 dev eth0\n",
		routeGet: "10.213.1.1 dev eth0 src 10.213.1.2 uid 0 \n    cache \n",
		inspect:  `{"slot":1,"vm":"10.213.1.2","gateway":"10.213.1.1","running":true}` + "\n",
		exec:     func(_ vmexec.Cmd, p *fakeProc) { p.exit(nil) },
	}
}

// fakeExit is an exit status, as *exec.ExitError reports it.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

type fakeProc struct {
	done    chan struct{}
	once    sync.Once
	err     error
	stopped bool
}

func (p *fakeProc) exit(err error) { p.once.Do(func() { p.err = err; close(p.done) }) }

func (p *fakeProc) Exited() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error              { <-p.done; return p.err }
func (p *fakeProc) Stop()                   { p.stopped = true; p.exit(errors.New("signal: terminated")) }

// verb is the helper verb of a call and its arguments.
func verb(c vmexec.Cmd) (string, []string) {
	if c.Name != "sudo" || len(c.Args) < 3 || c.Args[0] != "-n" || c.Args[1] != katahelper.HelperPath {
		return "", nil
	}
	return c.Args[2], c.Args[3:]
}

func (f *fakeHelper) record(c vmexec.Cmd, suffix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.Join(append([]string{c.Name}, c.Args...), " ") + suffix
	if c.Stdin != nil {
		call += " <stdin"
	}
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.Contains(call, f.fail) {
		return errors.New(f.failErr)
	}
	return nil
}

func (f *fakeHelper) Start(c vmexec.Cmd) (host.Process, error) {
	if err := f.record(c, " &"); err != nil {
		return nil, err
	}
	if v, _ := verb(c); v != "exec" {
		return nil, fmt.Errorf("unexpected start %v", c.Args)
	}
	p := &fakeProc{done: make(chan struct{})}
	f.mu.Lock()
	f.lastExec = p
	f.mu.Unlock()
	go f.exec(c, p)
	return p, nil
}

func (f *fakeHelper) Run(ctx context.Context, c vmexec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record(c, ""); err != nil {
		_, _ = io.WriteString(c.Stderr, "kata-helper: "+err.Error()+"\n")
		return fakeExit(1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, a := verb(c)
	switch v {
	case "create":
		f.ws[a[0]] = "created"
		_, _ = io.WriteString(c.Stdout, `{"slot":1,"vm":"10.213.1.2","gateway":"10.213.1.1"}`+"\n")
		return nil
	case "start":
		f.ws[a[0]] = "running"
		return nil
	case "stop":
		f.ws[a[0]] = "stopped"
		return nil
	case "rm":
		delete(f.ws, a[0])
		return nil
	case "inspect":
		_, _ = io.WriteString(c.Stdout, f.inspect)
		return nil
	case "list":
		for _, n := range slices.Sorted(func(yield func(string) bool) {
			for n := range f.ws {
				if !yield(n) {
					return
				}
			}
		}) {
			_, _ = fmt.Fprintln(c.Stdout, n)
		}
		_, _ = fmt.Fprintln(c.Stdout, "ws-gw")
		return nil
	case "exec":
		if a[0] == "-i" {
			a = a[1:]
		}
		if f.ws[a[0]] != "running" {
			_, _ = io.WriteString(c.Stderr, "kata-helper: "+a[0]+" isn't running\n")
			return fakeExit(katahelper.ExecFailed)
		}
		return f.execRun(c, a[1:])
	}
	return fmt.Errorf("unexpected call %v", c.Args)
}

// execRun answers a command Run through kata-helper exec; f.mu is held.
func (f *fakeHelper) execRun(c vmexec.Cmd, cmd []string) error {
	switch {
	case slices.Equal(cmd, []string{"test", "-S", "/run/systemd/private"}):
		if f.neverBoot || f.bootFails > 0 {
			f.bootFails--
			return fakeExit(1)
		}
		return nil
	case slices.Equal(cmd, []string{"ip", "-4", "route", "show", "default"}):
		_, _ = io.WriteString(c.Stdout, f.routes)
		return nil
	case len(cmd) == 5 && slices.Equal(cmd[:4], []string{"ip", "-4", "route", "get"}):
		_, _ = io.WriteString(c.Stdout, f.routeGet)
		return nil
	case slices.Equal(cmd, []string{"sudo", "-n", "ln", "-sfn", AgentDir + "/factory-agent", AgentLink}):
		return nil
	case len(cmd) == 6 && cmd[0] == "sudo" && cmd[4] == vmexec.PutScript:
		tr := tar.NewReader(c.Stdin)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			b, _ := io.ReadAll(tr)
			f.put = append(f.put, fmt.Sprintf("%s %o %q", h.Name, h.Mode, b))
		}
	case len(cmd) > 2 && cmd[0] == "sudo" && cmd[2] == "tar":
		_, err := io.Copy(c.Stdout, bytes.NewReader(f.get))
		return err
	case len(cmd) == 5 && cmd[2] == vmexec.SignalScript:
		f.signals = append(f.signals, cmd[4]+" "+cmd[3])
		if f.sig != nil {
			f.sig <- cmd[4]
		}
		return nil
	}
	return fmt.Errorf("unexpected exec %v", cmd)
}
