package tart

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/compliance-framework/factory/internal/agent/host"
)

// fakeTart answers the tart calls the driver makes and records them; nothing runs the real
// tart. A call containing fail fails with failErr on stderr.
type fakeTart struct {
	mu        sync.Mutex
	calls     []string
	vms       map[string]string // local VM -> stopped | running
	runs      map[string]*fakeProc
	bootFails int    // tart exec <vm> true fails this many times first
	runExits  bool   // tart run exits at once
	neverBoot bool   // tart exec <vm> true always fails
	routes    string // ip -4 route show default's answer
	routeGet  string // ip -4 route get's answer
	fail      string
	failErr   string
	put       []string // the tar entries Put sent: "name mode"
	get       []byte   // tar -czf's answer
	signals   []string
	sig       chan string // gets each signal sent, when set (buffered)
	// exec is what an Exec'd command does; it gets the call and its process.
	exec func(c Cmd, p *fakeProc)
	// lastExec is the last Exec'd command's process.
	lastExec *fakeProc
}

func newFake() *fakeTart {
	return &fakeTart{
		vms:      map[string]string{"factory-agent-warm": "stopped", "ws-gw": "running", "ws-1": "stopped"},
		runs:     map[string]*fakeProc{},
		routes:   "default via 192.168.96.33 dev enp0s1 proto dhcp src 192.168.96.34 metric 100\n",
		routeGet: "192.168.96.33 dev enp0s1 src 192.168.96.34 uid 501 \n    cache \n",
		exec:     func(_ Cmd, p *fakeProc) { p.exit(nil) },
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
	onStop  func() // tart run's: SIGTERM shuts its VM down
}

func newProc() *fakeProc { return &fakeProc{done: make(chan struct{})} }

func (p *fakeProc) exit(err error) {
	p.once.Do(func() { p.err = err; close(p.done) })
}

func (p *fakeProc) Exited() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error              { <-p.done; return p.err }
func (p *fakeProc) Stop() {
	p.stopped = true
	if p.onStop != nil {
		p.onStop()
	}
	p.exit(errors.New("signal: terminated"))
}

func describe(c Cmd) string {
	s := strings.Join(append([]string{c.Name}, c.Args...), " ")
	if len(c.Env) > 0 {
		s = strings.Join(c.Env, " ") + " " + s
	}
	if c.Stdin != nil {
		s += " <stdin"
	}
	return s
}

func (f *fakeTart) record(c Cmd, suffix string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := describe(c) + suffix
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.Contains(call, f.fail) {
		return call, errors.New(f.failErr)
	}
	return call, nil
}

func (f *fakeTart) Start(c Cmd) (host.Process, error) {
	if _, err := f.record(c, " &"); err != nil {
		return nil, err
	}
	p := newProc()
	switch c.Args[0] {
	case "run":
		f.mu.Lock()
		vm := c.Args[1]
		if f.vms[vm] != "stopped" {
			f.mu.Unlock()
			return nil, fmt.Errorf("unexpected run of %s (%s)", vm, f.vms[vm])
		}
		f.vms[vm], f.runs[vm] = "running", p
		p.onStop = func() { f.stopVM(vm) }
		f.mu.Unlock()
		if f.runExits {
			f.stopVM(vm)
		}
	case "exec":
		f.mu.Lock()
		f.lastExec = p
		f.mu.Unlock()
		go f.exec(c, p)
	default:
		return nil, fmt.Errorf("unexpected start %s", describe(c))
	}
	return p, nil
}

// stopVM stops vm and ends its tart run.
func (f *fakeTart) stopVM(vm string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vms[vm]; ok {
		f.vms[vm] = "stopped"
	}
	if p := f.runs[vm]; p != nil {
		p.exit(fakeExit(1))
		delete(f.runs, vm)
	}
}

func (f *fakeTart) Run(_ context.Context, c Cmd) error {
	call, err := f.record(c, "")
	if err != nil {
		_, _ = io.WriteString(c.Stderr, f.failErr+"\n")
		return fakeExit(1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := c.Args
	running := func(vm string) error {
		if f.vms[vm] != "running" {
			return fmt.Errorf("%s isn't running", vm)
		}
		return nil
	}
	switch a[0] {
	case "list":
		var list []map[string]string
		list = append(list, map[string]string{"Name": "ghcr.io/cirruslabs/ubuntu:latest", "State": "stopped", "Source": "OCI"})
		for _, n := range slices.Sorted(func(yield func(string) bool) {
			for n := range f.vms {
				if !yield(n) {
					return
				}
			}
		}) {
			list = append(list, map[string]string{"Name": n, "State": f.vms[n], "Source": "local"})
		}
		return json.NewEncoder(c.Stdout).Encode(list)
	case "clone":
		if _, ok := f.vms[a[1]]; !ok {
			return fmt.Errorf("no VM %s", a[1])
		}
		f.vms[a[2]] = "stopped"
		return nil
	case "set":
		return nil
	case "delete":
		delete(f.vms, a[1])
		return nil
	case "stop":
		if f.vms[a[1]] != "running" {
			_, _ = io.WriteString(c.Stderr, "VM \""+a[1]+"\" is not running\n")
			return fakeExit(1)
		}
		f.vms[a[1]] = "stopped"
		if p := f.runs[a[1]]; p != nil {
			p.exit(nil)
			delete(f.runs, a[1])
		}
		return nil
	case "exec":
		i := 1
		if a[1] == "-i" {
			i = 2
		}
		vm, cmd := a[i], a[i+1:]
		if err := running(vm); err != nil {
			return err
		}
		switch {
		case slices.Equal(cmd, []string{"true"}):
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
		case len(cmd) == 6 && cmd[0] == "sudo" && cmd[4] == putScript:
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
				f.put = append(f.put, fmt.Sprintf("%s %o uid=%d %q", h.Name, h.Mode, h.Uid, b))
			}
		case len(cmd) > 2 && cmd[0] == "sudo" && cmd[2] == "tar":
			_, err := io.Copy(c.Stdout, bytes.NewReader(f.get))
			return err
		case len(cmd) == 5 && cmd[2] == signalScript:
			f.signals = append(f.signals, cmd[4]+" "+cmd[3])
			if f.sig != nil {
				f.sig <- cmd[4]
			}
			return nil
		}
	}
	return fmt.Errorf("unexpected call %s", call)
}
