package host

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/clock"
)

// fakeTart is tart's VMs: clone, set, run (through Start), exec, stop, rename, delete, list.
// The fakeRunner's mutex guards it.
type fakeTart struct {
	vms       map[string]string // local VM -> stopped | running
	procs     map[string]*fakeProc
	bootFails int    // tart exec <vm> true fails this many times first
	noStop    bool   // shutdown -h doesn't stop the VM
	runExits  bool   // tart run exits at once
	fail      string // a tart call containing it fails
	script    []byte // what bash -s read
	files     map[string][]byte
	badSum    bool // sha256sum answers another sum
	// listLag: after shutdown -h, tart list still shows the VM running this many times,
	// though tart run has exited (a real Mac's order).
	listLag int
	lag     map[string]int
	// stopStrict: tart stop on a VM that isn't running fails, as tart 2.41 does.
	stopStrict bool
}

func (f *fakeTart) init() {
	f.vms, f.procs, f.files, f.lag = map[string]string{}, map[string]*fakeProc{}, map[string][]byte{}, map[string]int{}
}

type fakeProc struct {
	r    *fakeRunner
	vm   string
	done chan struct{}
}

func (p *fakeProc) Exited() <-chan struct{} { return p.done }

func (p *fakeProc) Err() error { <-p.done; return errors.New("signal: terminated") }

func (p *fakeProc) Stop() {
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	p.r.tart.stop(p.vm)
}

// stop stops vm and ends its tart run.
func (f *fakeTart) stop(vm string) {
	if _, ok := f.vms[vm]; ok {
		f.vms[vm] = "stopped"
	}
	if p := f.procs[vm]; p != nil {
		close(p.done)
		delete(f.procs, vm)
	}
}

func (r *fakeRunner) RunInput(_ context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ") + " <stdin"
	r.calls = append(r.calls, call)
	return r.tart.run(call, out, in, args)
}

func (r *fakeRunner) Start(_ context.Context, _ io.Writer, name string, args ...string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ") + " &"
	r.calls = append(r.calls, call)
	f := &r.tart
	if f.fail != "" && strings.Contains(call, f.fail) {
		return nil, errors.New("injected failure")
	}
	if len(args) < 2 || args[0] != "run" || f.vms[args[1]] != "stopped" {
		return nil, fmt.Errorf("unexpected start %s", call)
	}
	p := &fakeProc{r: r, vm: args[1], done: make(chan struct{})}
	f.vms[args[1]], f.procs[args[1]] = "running", p
	if f.runExits {
		f.stop(args[1])
	}
	return p, nil
}

func (f *fakeTart) run(call string, out io.Writer, in io.Reader, args []string) error {
	if f.fail != "" && strings.Contains(call, f.fail) {
		return errors.New("injected failure")
	}
	need := func(vm string) error {
		if _, ok := f.vms[vm]; !ok {
			return fmt.Errorf("the VM %q does not exist", vm)
		}
		return nil
	}
	switch args[0] {
	case "list":
		type vm struct{ Name, State, Source string }
		list := []vm{{Name: "ghcr.io/cirruslabs/ubuntu:latest", State: "stopped", Source: "OCI"}}
		for _, n := range slices.Sorted(func(yield func(string) bool) {
			for n := range f.vms {
				if !yield(n) {
					return
				}
			}
		}) {
			st := f.vms[n]
			if f.lag[n] > 0 {
				f.lag[n]--
				st = "running"
			}
			list = append(list, vm{n, st, "local"})
		}
		return json.NewEncoder(out).Encode(list)
	case "clone":
		if !strings.Contains(args[1], "@sha256:") {
			if err := need(args[1]); err != nil {
				return err
			}
		}
		if _, ok := f.vms[args[2]]; ok {
			return fmt.Errorf("the VM %q already exists", args[2])
		}
		f.vms[args[2]] = "stopped"
	case "set":
		return need(args[1])
	case "delete":
		if err := need(args[1]); err != nil {
			return err
		}
		if f.vms[args[1]] == "running" {
			return fmt.Errorf("the VM %q is running", args[1])
		}
		delete(f.vms, args[1])
		f.moveFiles(args[1], "")
	case "rename":
		if err := need(args[1]); err != nil {
			return err
		}
		if _, ok := f.vms[args[2]]; ok {
			return fmt.Errorf("the VM %q already exists", args[2])
		}
		f.vms[args[2]] = f.vms[args[1]]
		delete(f.vms, args[1])
		f.moveFiles(args[1], args[2])
	case "stop":
		if f.stopStrict && f.vms[args[1]] != "running" {
			_, _ = fmt.Fprintf(out, "VM %q is not running\n", args[1])
			return errors.New("exit status 2")
		}
		f.stop(args[1])
	case "exec":
		args = args[1:]
		if args[0] == "-i" {
			args = args[1:]
		}
		vm, cmd := args[0], strings.Join(args[1:], " ")
		if f.vms[vm] != "running" {
			return fmt.Errorf("the VM %q is not running", vm)
		}
		switch {
		case cmd == "true" && f.bootFails > 0:
			f.bootFails--
			return errors.New("no guest agent yet")
		case cmd == "bash -s":
			b, err := io.ReadAll(in)
			f.script = b
			return err
		case cmd == "sudo tar -xf - -C /usr/local/bin":
			tr := tar.NewReader(in)
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				b, err := io.ReadAll(tr)
				if err != nil {
					return err
				}
				f.files[vm+":/usr/local/bin/"+h.Name] = b
			}
		case strings.HasPrefix(cmd, "sha256sum "):
			b, ok := f.files[vm+":"+args[2]]
			if !ok {
				return errors.New("no such file")
			}
			if f.badSum {
				b = append(b, 'x')
			}
			_, _ = fmt.Fprintf(out, "%s  %s\n", sum(b), args[2])
		case cmd == "sudo shutdown -h now":
			if !f.noStop {
				f.stop(vm)
				f.lag[vm] = f.listLag
			}
			return errors.New("connection lost")
		}
	default:
		return fmt.Errorf("unexpected tart %s", call)
	}
	return nil
}

// moveFiles moves vm's files to to (deletes them when to is ""): a VM's disk goes with it.
func (f *fakeTart) moveFiles(vm, to string) {
	for k, b := range f.files {
		if p, ok := strings.CutPrefix(k, vm+":"); ok {
			delete(f.files, k)
			if to != "" {
				f.files[to+":"+p] = b
			}
		}
	}
}

// tartCalls are the recorded tart calls.
func (r *fakeRunner) tartCalls() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, c := range r.calls {
		if strings.HasPrefix(c, "tart ") {
			b.WriteString(c + "\n")
		}
	}
	return b.String()
}

func (r *fakeRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

func (r *fakeRunner) vmState(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tart.vms[name]
}

func readBaseRecord(t *testing.T, e *Env) BaseVMRecord {
	t.Helper()
	var rec BaseVMRecord
	if ok, err := readRecord(baseVMStep{e}.record(), &rec); err != nil || !ok {
		t.Fatalf("basevm.json: %v %v", ok, err)
	}
	return rec
}

func mkStateDir(t *testing.T, e *Env) {
	t.Helper()
	if err := (stateDirStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBaseVMName(t *testing.T) {
	for v, want := range map[string]string{"v0.1.0": "factory-agent-base-0.1.0", "0.2.0-rc.1": "factory-agent-base-0.2.0-rc.1", "dev": "factory-agent-base-dev", "v1+meta/x": "factory-agent-base-1_meta_x"} {
		if got := (&Env{AgentVersion: v}).BaseVMName(); got != want {
			t.Errorf("BaseVMName(%q) = %q, want %q", v, got, want)
		}
	}
}

// TestBaseVMApply builds the base: the exact tart sequence, the script and the binary piped
// in, the record, and a check that is ok after.
func TestBaseVMApply(t *testing.T) {
	e, r := testEnv(t)
	mkStateDir(t, e)
	r.tart.bootFails = 1
	// a leftover .building and an older base with the same name are replaced.
	r.tart.vms["factory-agent-base-0.1.0.building"] = "stopped"
	r.tart.vms["factory-agent-base-0.1.0"] = "stopped"
	r.tart.vms["factory-ws-base"] = "stopped"
	s := baseVMStep{e}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	golden(t, "basevm-build.golden", e, r.tartCalls())
	if !bytes.Equal(r.tart.script, provisionScript) {
		t.Error("bash -s didn't get the provisioning script")
	}
	if got := string(r.tart.files["factory-agent-base-0.1.0:/usr/local/bin/factory-agent"]); got != "factory-agent linux/arm64" {
		t.Errorf("the VM's factory-agent is %q", got)
	}
	if r.vmState("factory-agent-base-0.1.0") != "stopped" || r.vmState("factory-agent-base-0.1.0.building") != "" || r.vmState("factory-ws-base") != "stopped" {
		t.Errorf("VMs %v", r.tart.vms)
	}
	rec := readBaseRecord(t, e)
	agent, _ := os.ReadFile(e.AgentLinux)
	if want := (BaseVMRecord{Name: "factory-agent-base-0.1.0", Source: BaseImage, ScriptSHA256: sum(provisionScript), AgentSHA256: sum(agent), AgentVersion: "v0.1.0", BuiltAt: "2026-10-22T09:30:00Z"}); rec != want {
		t.Errorf("record %+v\nwant %+v", rec, want)
	}
	if fi, err := os.Stat(s.record()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("basevm.json: %v %v", fi, err)
	}
	wantState(t, check(t, s), StateOK, "factory-agent-base-0.1.0 (built 2026-10-22T09:30:00Z)")
}

func TestBaseVMCheck(t *testing.T) {
	built := func(t *testing.T, e *Env, r *fakeRunner) {
		t.Helper()
		mkStateDir(t, e)
		if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(f func(*BaseVMRecord)) func(*testing.T, *Env, *fakeRunner) {
		return func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			rec := readBaseRecord(t, e)
			f(&rec)
			if err := writeRecord(baseVMStep{e}.record(), rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name   string
		set    func(*testing.T, *Env, *fakeRunner)
		want   State
		detail string
	}{
		{"no linux binary", func(_ *testing.T, e *Env, _ *fakeRunner) { e.AgentLinux = filepath.Join(e.Home, "nope") }, StateWrong, "build it with make tart-up"},
		{"no record", func(*testing.T, *Env, *fakeRunner) {}, StateMissing, "basevm.json"},
		{"ok", built, StateOK, "factory-agent-base-0.1.0"},
		{"no VM", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			delete(r.tart.vms, "factory-agent-base-0.1.0")
		}, StateMissing, "no VM factory-agent-base-0.1.0"},
		{"running", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			r.tart.vms["factory-agent-base-0.1.0"] = "running"
		}, StateWrong, "is running"},
		{"new version", func(t *testing.T, e *Env, r *fakeRunner) { built(t, e, r); e.AgentVersion = "v0.2.0" }, StateOutdated, "name is factory-agent-base-0.1.0, want factory-agent-base-0.2.0"},
		{"source", edit(func(r *BaseVMRecord) { r.Source = "ghcr.io/cirruslabs/ubuntu@sha256:00" }), StateOutdated, "source is ghcr.io/cirruslabs/ubuntu@sha256:00"},
		{"script", edit(func(r *BaseVMRecord) { r.ScriptSHA256 = "old" }), StateOutdated, "scriptSHA256 is old"},
		{"agent", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			if err := os.WriteFile(e.AgentLinux, []byte("a newer build"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, StateOutdated, "agentSHA256 is"},
		{"corrupt record", func(t *testing.T, e *Env, r *fakeRunner) {
			mkStateDir(t, e)
			_ = os.WriteFile(baseVMStep{e}.record(), []byte("{"), 0o600)
		}, StateWrong, "basevm.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			tt.set(t, e, r)
			wantState(t, Check(context.Background(), []Step{baseVMStep{e}})[0], tt.want, tt.detail)
		})
	}
}

// TestBaseVMApplyFails: a failure at any point deletes the .building VM (after stopping it)
// and writes no record; --keep-failed keeps it.
func TestBaseVMApplyFails(t *testing.T) {
	tests := []struct {
		name, fail string
		set        func(*Env, *fakeRunner)
		keep       bool
		wantErr    string
	}{
		{"no linux binary", "", func(e *Env, _ *fakeRunner) { e.AgentLinux = "" }, false, "build it with make tart-up"},
		{"clone", "tart clone", nil, false, "injected failure"},
		{"set", "tart set", nil, false, "injected failure"},
		{"run", "tart run", nil, false, "injected failure"},
		{"tart run exits", "", func(_ *Env, r *fakeRunner) { r.tart.runExits = true }, false, "tart run exited before its guest agent answered: signal: terminated"},
		{"boot timeout", "", func(e *Env, r *fakeRunner) { r.tart.bootFails = 1 << 30; e.VM.Boot = 20 * time.Millisecond }, false, "timed out after 20ms waiting for its guest agent"},
		{"provisioning", "bash -s", nil, false, "the provisioning script: injected failure"},
		{"provisioning, kept", "bash -s", nil, true, "the provisioning script"},
		{"copy", "sudo tar", nil, false, "copying factory-agent"},
		{"sha256 in the VM", "", func(_ *Env, r *fakeRunner) { r.tart.badSum = true }, false, "want the host file's"},
		{"rename", "tart rename", nil, false, "injected failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			mkStateDir(t, e)
			var out bytes.Buffer
			e.Out, e.KeepFailed, r.tart.fail = &out, tt.keep, tt.fail
			if tt.set != nil {
				tt.set(e, r)
			}
			err := baseVMStep{e}.Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			b := "factory-agent-base-0.1.0.building"
			switch st := r.vmState(b); {
			case tt.keep && st != "stopped":
				t.Errorf("--keep-failed: %s is %q, want it kept, stopped", b, st)
			case tt.keep && !strings.Contains(out.String(), "--keep-failed: kept "+b):
				t.Errorf("output %q", out.String())
			case !tt.keep && st != "":
				t.Errorf("%s is left %q", b, st)
			}
			if _, err := os.Stat(baseVMStep{e}.record()); err == nil {
				t.Error("a failed build wrote basevm.json")
			}
		})
	}
}

// TestBaseVMLeftoverRunning: a .building VM left running by a killed factory-agent is stopped
// before it is deleted.
func TestBaseVMLeftoverRunning(t *testing.T) {
	e, r := testEnv(t)
	mkStateDir(t, e)
	r.tart.vms["factory-agent-base-0.1.0.building"] = "running"
	if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := r.tartCalls()
	stop, del := strings.Index(calls, "tart stop factory-agent-base-0.1.0.building\n"), strings.Index(calls, "tart delete factory-agent-base-0.1.0.building\n")
	if stop < 0 || del < stop {
		t.Errorf("want tart stop then tart delete of the leftover:\n%s", calls)
	}
}

// TestTartBin: the VM steps run the tart the tart step accepts.
func TestTartBin(t *testing.T) {
	tests := []struct {
		name, state, onPath, want string
	}{
		{"the state dir's at the pin", TartVersion, "2.42.0", "STATE"},
		{"the state dir's outdated, PATH's newer", "2.30.0", "2.42.0", "/opt/homebrew/bin/tart"},
		{"only PATH's", "", "2.42.0", "/opt/homebrew/bin/tart"},
		{"none", "", "", "tart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			s := tartStep{e}
			if tt.state != "" {
				if err := os.MkdirAll(filepath.Dir(s.bin()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.bin(), nil, 0o755); err != nil {
					t.Fatal(err)
				}
				r.versions[s.bin()] = tt.state
			}
			if tt.onPath != "" {
				e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
			}
			want := strings.ReplaceAll(tt.want, "STATE", s.bin())
			if got := e.tartBin(context.Background()); got != want {
				t.Errorf("tartBin = %q, want %q", got, want)
			}
		})
	}
}

// TestBaseVMShutdownTimeout: a VM that doesn't stop after shutdown gets tart stop.
func TestBaseVMShutdownTimeout(t *testing.T) {
	e, r := testEnv(t)
	mkStateDir(t, e)
	r.tart.noStop = true
	e.VM.Shutdown = 20 * time.Millisecond
	if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.ran("tart stop factory-agent-base-0.1.0.building") != 1 || r.vmState("factory-agent-base-0.1.0") != "stopped" {
		t.Errorf("calls %v", r.calls)
	}
}

// TestBaseVMShutdown: tart run exits when the guest powers off, often before tart list says
// stopped; and tart stop fails on a VM that has stopped. Neither fails the build.
func TestBaseVMShutdown(t *testing.T) {
	tests := []struct {
		name     string
		set      func(*Env, *fakeRunner)
		wantStop int
		wantOut  string
		wantErr  string
	}{
		{"tart run exits before tart list says stopped", func(_ *Env, r *fakeRunner) { r.tart.listLag, r.tart.stopStrict = 3, true }, 0, "tart run exited: waiting for tart list", ""},
		{"tart stop on a VM that stopped meanwhile", func(e *Env, r *fakeRunner) {
			r.tart.listLag, r.tart.stopStrict, e.VM.Shutdown = 1<<30, true, 20*time.Millisecond
		}, 1, `VM "factory-agent-base-0.1.0.building" is not running`, ""},
		{"tart stop fails", func(e *Env, r *fakeRunner) { r.tart.noStop, r.tart.fail, e.VM.Shutdown = true, "tart stop", 20*time.Millisecond }, 1, "didn't stop within 20ms", "injected failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			mkStateDir(t, e)
			var out bytes.Buffer
			e.Out = &out
			tt.set(e, r)
			err := baseVMStep{e}.Apply(context.Background())
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("%v\n%s", err, out.String())
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if n := r.ran("tart stop"); n != tt.wantStop || !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("tart stop ran %d times, want %d; output:\n%s", n, tt.wantStop, out.String())
			}
			if tt.wantErr == "" && r.vmState("factory-agent-base-0.1.0") != "stopped" {
				t.Errorf("VMs %v", r.tart.vms)
			}
		})
	}
}

// TestBaseVMReplace: an older VM is renamed aside and deleted only once the new one is in
// place; a failed rename puts it back.
func TestBaseVMReplace(t *testing.T) {
	const final, old, building = "factory-agent-base-0.1.0", "factory-agent-base-0.1.0.old", "factory-agent-base-0.1.0.building"
	tests := []struct {
		name    string
		vms     []string // the VMs before the build, each marked as the older one
		fail    string
		wantErr string
		want    string // what final is after: "new", "older"
	}{
		{"replaces the older", []string{final}, "", "", "new"},
		{"rename fails: the older stays", []string{final}, "tart rename " + building, "injected failure", "older"},
		{"rename aside fails", []string{final}, "tart rename " + final + " ", "injected failure", "older"},
		{"an interrupted run's .old is the older", []string{old}, "tart rename " + building, "injected failure", "older"},
		{"an interrupted run's .old, replaced", []string{old}, "", "", "new"},
		{"a leftover .old beside the older", []string{final, old}, "", "", "new"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			mkStateDir(t, e)
			for _, v := range tt.vms {
				r.tart.vms[v] = "stopped"
				r.tart.files[v+":id"] = []byte("older")
			}
			r.tart.fail = tt.fail
			if err := (baseVMStep{e}).Apply(context.Background()); (tt.wantErr == "" && err != nil) || (tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr))) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			got := "missing"
			switch {
			case r.vmState(final) == "":
			case string(r.tart.files[final+":id"]) == "older":
				got = "older"
			case r.tart.files[final+":/usr/local/bin/factory-agent"] != nil:
				got = "new"
			}
			if got != tt.want || r.vmState(old) != "" || r.vmState(building) != "" {
				t.Errorf("%s is %s, want %s; VMs %v; calls:\n%s", final, got, tt.want, r.tart.vms, r.tartCalls())
			}
		})
	}
}

// TestBaseVMPreviousVersion: a new version's base deletes the previous one basevm.json named,
// unless it's running or not one of host init's.
func TestBaseVMPreviousVersion(t *testing.T) {
	tests := []struct {
		name, prev, state string
		gone              bool
		wantOut           string
	}{
		{"deleted", "factory-agent-base-0.1.0", "stopped", true, "deleting the previous base factory-agent-base-0.1.0"},
		{"running", "factory-agent-base-0.1.0", "running", false, "warning: the previous base factory-agent-base-0.1.0 is running"},
		{"not host init's", "factory-ws-base", "stopped", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			mkStateDir(t, e)
			r.tart.vms[tt.prev] = tt.state
			if err := writeRecord(baseVMStep{e}.record(), BaseVMRecord{Name: tt.prev}); err != nil {
				t.Fatal(err)
			}
			e.AgentVersion = "v0.2.0"
			var out bytes.Buffer
			e.Out = &out
			p, err := baseVMStep{e}.Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			planned := slices.Contains(p.Do, "tart delete "+tt.prev+" (the previous base, named in basevm.json), unless it's running")
			if planned != (tt.prev != "factory-ws-base") {
				t.Errorf("plan %q", p.Do)
			}
			if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if gone := r.vmState(tt.prev) == ""; gone != tt.gone || r.vmState("factory-agent-base-0.2.0") != "stopped" {
				t.Errorf("VMs %v", r.tart.vms)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output:\n%s", out.String())
			}
			if rec := readBaseRecord(t, e); rec.Name != "factory-agent-base-0.2.0" {
				t.Errorf("record %+v", rec)
			}
		})
	}
}

// TestBaseVMInterrupted: an interrupt during provisioning still stops and deletes the VM.
func TestBaseVMInterrupted(t *testing.T) {
	e, r := testEnv(t)
	mkStateDir(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	r.tart.bootFails = 1 << 30
	go func() {
		for r.ran("tart exec factory-agent-base-0.1.0.building true") == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := (baseVMStep{e}).Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if st := r.vmState("factory-agent-base-0.1.0.building"); st != "" {
		t.Errorf("the .building VM is left %q", st)
	}
}

func warmEnv(t *testing.T) (*Env, *fakeRunner) {
	t.Helper()
	e, r := testEnv(t)
	mkStateDir(t, e)
	if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.reset()
	e.Workspace.Warm.Images = []string{"postgres:17.5"}
	e.Workspace.Warm.KindImages = []string{"ghcr.io/x/agent:0.7.1@sha256:ab"}
	return e, r
}

func TestWarmBaseApply(t *testing.T) {
	e, r := warmEnv(t)
	r.tart.vms[WarmBaseVM] = "stopped" // an older warm base is replaced
	s := warmBaseStep{e}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	golden(t, "warmbase-build.golden", e, r.tartCalls())
	var rec WarmBaseRecord
	if ok, err := readRecord(s.record(), &rec); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if want := (WarmBaseRecord{Base: "factory-agent-base-0.1.0", BaseBuiltAt: "2026-10-22T09:30:00Z", ImagesSHA256: s.imagesSHA256(), BuiltAt: "2026-10-22T09:30:00Z"}); rec != want {
		t.Errorf("record %+v\nwant %+v", rec, want)
	}
	wantState(t, check(t, s), StateOK, "factory-agent-warm from factory-agent-base-0.1.0, 1 images, 1 kind images")
}

// TestWarmBasePlainClone: without warm images the warm base is a clone, never booted.
func TestWarmBasePlainClone(t *testing.T) {
	e, r := warmEnv(t)
	e.Workspace.Warm = config.WarmImages{}
	if err := (warmBaseStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "tart list --format json\ntart list --format json\ntart clone factory-agent-base-0.1.0 factory-agent-warm.building\ntart list --format json\ntart rename factory-agent-warm.building factory-agent-warm\n"
	if got := r.tartCalls(); got != want {
		t.Errorf("calls:\n%s\nwant:\n%s", got, want)
	}
}

func TestWarmBaseCheck(t *testing.T) {
	built := func(t *testing.T, e *Env, _ *fakeRunner) {
		t.Helper()
		if err := (warmBaseStep{e}).Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		set    func(*testing.T, *Env, *fakeRunner)
		want   State
		detail string
	}{
		{"no record", func(*testing.T, *Env, *fakeRunner) {}, StateMissing, "warmbase.json"},
		{"ok", built, StateOK, "factory-agent-warm from factory-agent-base-0.1.0"},
		{"no VM", func(t *testing.T, e *Env, r *fakeRunner) { built(t, e, r); delete(r.tart.vms, WarmBaseVM) }, StateMissing, "no VM factory-agent-warm"},
		{"base outdated", func(t *testing.T, e *Env, r *fakeRunner) { built(t, e, r); e.AgentVersion = "v0.2.0" }, StateOutdated, "the base VM is outdated"},
		{"base rebuilt", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			e.Clock = clock.NewFake(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
			if err := (baseVMStep{e}).Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, StateOutdated, "baseBuiltAt is 2026-10-22T09:30:00Z, want 2026-10-23T00:00:00Z"},
		{"base renamed", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			var rec WarmBaseRecord
			_, _ = readRecord(warmBaseStep{e}.record(), &rec)
			rec.Base = "factory-agent-base-0.0.9"
			_ = writeRecord(warmBaseStep{e}.record(), rec)
		}, StateOutdated, "base is factory-agent-base-0.0.9"},
		{"images changed", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			e.Workspace.Warm.Images = append(e.Workspace.Warm.Images, "redis:8")
		}, StateOutdated, "imagesSHA256 is"},
		{"image moved to kind", func(t *testing.T, e *Env, r *fakeRunner) {
			built(t, e, r)
			e.Workspace.Warm.Images, e.Workspace.Warm.KindImages = nil, []string{"postgres:17.5", "ghcr.io/x/agent:0.7.1@sha256:ab"}
		}, StateOutdated, "imagesSHA256 is"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := warmEnv(t)
			tt.set(t, e, r)
			wantState(t, check(t, warmBaseStep{e}), tt.want, tt.detail)
		})
	}
}

func TestWarmBaseApplyFails(t *testing.T) {
	t.Run("pull", func(t *testing.T) {
		e, r := warmEnv(t)
		r.tart.fail = "docker pull postgres"
		if err := (warmBaseStep{e}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "sudo docker pull postgres:17.5") {
			t.Fatalf("err = %v", err)
		}
		if st := r.vmState(WarmBaseVM + buildingSuffix); st != "" {
			t.Errorf("the .building VM is left %q", st)
		}
		if _, err := os.Stat(warmBaseStep{e}.record()); err == nil {
			t.Error("a failed build wrote warmbase.json")
		}
	})
	t.Run("no base", func(t *testing.T) {
		e, r := testEnv(t)
		mkStateDir(t, e)
		if err := (warmBaseStep{e}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "the base VM is missing") || !strings.Contains(err.Error(), "build it first") {
			t.Fatalf("err = %v", err)
		}
		if r.ran("tart clone") != 0 {
			t.Errorf("calls %v", r.calls)
		}
	})
	t.Run("stale base", func(t *testing.T) {
		e, r := warmEnv(t)
		e.AgentVersion = "v0.2.0" // the base is factory-agent-base-0.1.0: outdated
		if err := (warmBaseStep{e}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "the base VM is outdated") {
			t.Fatalf("err = %v", err)
		}
		if r.ran("tart clone") != 0 {
			t.Errorf("a stale base was cloned: %v", r.calls)
		}
	})
	t.Run("archive collision", func(t *testing.T) {
		e, _ := warmEnv(t)
		e.Workspace.Warm.KindImages = []string{"a/b:1", "a_b:1"}
		if _, err := (warmBaseStep{e}).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "both save to /var/lib/factory/kind-images/a_b_1.tar") {
			t.Fatalf("err = %v", err)
		}
	})
}

// planLines are a step's lines in Init's plan ("  <step>  ...", as Init prints them).
func planLines(out, step string) []string {
	var ls []string
	for l := range strings.Lines(out) {
		if strings.HasPrefix(l, fmt.Sprintf("  %-9s ", step)) {
			ls = append(ls, l)
		}
	}
	return ls
}

// TestInitOnly: --only applies only the named steps; the check still covers every step.
func TestInitOnly(t *testing.T) {
	e, r := testEnv(t)
	mkStateDir(t, e)
	var out bytes.Buffer
	e.Out = &out
	// the control: without --only, the plan has softnet's and pf's lines.
	if _, err := Init(context.Background(), e, InitOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(planLines(out.String(), "softnet")) == 0 || len(planLines(out.String(), "pf")) == 0 {
		t.Fatalf("the dry run's plan has no softnet or pf lines:\n%s", out.String())
	}
	out.Reset()
	sts, err := Init(context.Background(), e, InitOptions{Only: []string{"basevm", "warmbase"}})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(sts) != 7 || sts[5].State != StateOK || sts[6].State != StateOK || sts[3].State != StateMissing {
		t.Errorf("statuses %+v", sts)
	}
	if r.ran("sudo") != 0 || len(planLines(out.String(), "softnet")) > 0 || len(planLines(out.String(), "pf")) > 0 || len(planLines(out.String(), "tart")) > 0 {
		t.Errorf("--only ran other steps: calls %v\n%s", r.calls, out.String())
	}
	if _, err := Init(context.Background(), e, InitOptions{Only: []string{"basevm", "nope"}}); err == nil || !strings.Contains(err.Error(), `no step "nope"`) {
		t.Errorf("unknown step: %v", err)
	}
}

// TestInitOnlyStateDir: on a host without the state dir, --only applies the statedir step
// first, since the steps it names write there (and the host lock is there).
func TestInitOnlyStateDir(t *testing.T) {
	tests := []struct {
		name string
		o    InitOptions
		set  func(*testing.T, *Env, *fakeRunner)
		ok   func(*testing.T, *Env, []Status)
	}{
		{"tart", InitOptions{Only: []string{"tart"}}, func(t *testing.T, e *Env, _ *fakeRunner) {
			good := tgz(t, tartApp("ok")...)
			e.Pins.TartSHA256 = sum(good)
			e.Fetcher = newReleases(t, map[string][]byte{"tart.tar.gz": good})
		}, func(t *testing.T, e *Env, _ []Status) {
			if b, err := os.ReadFile(tartStep{e}.bin()); err != nil || string(b) != "tart binary" {
				t.Errorf("installed tart: %q %v", b, err)
			}
		}},
		{"pf with --no-pf", InitOptions{Only: []string{"pf"}, NoPF: true}, func(*testing.T, *Env, *fakeRunner) {}, func(t *testing.T, e *Env, sts []Status) {
			wantState(t, sts[4], StateSkipped, "--no-pf")
		}},
		{"pf, declined", InitOptions{Only: []string{"pf"}}, func(_ *testing.T, e *Env, _ *fakeRunner) {
			e.Prompt = func(string) (bool, error) { return false, nil }
		}, func(t *testing.T, e *Env, sts []Status) {
			wantState(t, sts[4], StateSkipped, "declined")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			var out bytes.Buffer
			e.Out = &out
			tt.set(t, e, r)
			sts, err := Init(context.Background(), e, tt.o)
			if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if len(planLines(out.String(), "statedir")) != 1 || sts[1].State != StateOK {
				t.Errorf("statedir: %+v\n%s", sts[1], out.String())
			}
			tt.ok(t, e, sts)
			if r.ran("tart clone") != 0 || r.ran("sudo") != 0 {
				t.Errorf("--only ran other steps: %v", r.calls)
			}
		})
	}
}

// TestInitLock: a second init fails while one holds the host lock; a dry run doesn't take it.
func TestInitLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no flock")
	}
	e, r := testEnv(t)
	mkStateDir(t, e)
	release, err := lockHost(e)
	if err != nil {
		t.Fatal(err)
	}
	o := InitOptions{Only: []string{"basevm"}}
	if _, err := Init(context.Background(), e, o); !errors.Is(err, ErrHostLocked) || !strings.Contains(err.Error(), "host.lock is held") {
		t.Fatalf("err = %v, want ErrHostLocked", err)
	}
	if r.ran("tart clone") != 0 {
		t.Errorf("a locked init built: %v", r.calls)
	}
	if _, err := Init(context.Background(), e, InitOptions{DryRun: true, Only: o.Only}); err != nil {
		t.Errorf("dry run: %v", err)
	}
	release()
	if _, err := Init(context.Background(), e, o); err != nil {
		t.Fatalf("after release: %v", err)
	}
	// Init released its lock.
	release, err = lockHost(e)
	if err != nil {
		t.Fatalf("Init kept the lock: %v", err)
	}
	release()
}

// TestInitRootBeforeVMs: the sudo script runs right after the last step with root lines, so
// a failing VM build doesn't skip it.
func TestInitRootBeforeVMs(t *testing.T) {
	e, r := testEnv(t)
	soft := tgz(t, entry{name: "softnet", body: "softnet binary"})
	e.Pins.SoftnetSHA256, e.Pins.SoftnetBinarySHA256 = sum(soft), sum([]byte("softnet binary"))
	e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
	r.versions["/opt/homebrew/bin/tart"] = TartVersion
	e.Fetcher = newReleases(t, map[string][]byte{"softnet.tar.gz": soft})
	r.tart.fail = "bash -s"
	if _, err := Init(context.Background(), e, InitOptions{NoPF: true}); err == nil || !strings.Contains(err.Error(), "basevm: the provisioning script") {
		t.Fatalf("err = %v", err)
	}
	if r.ran("sudo /bin/sh") != 1 {
		t.Errorf("calls %v, want the sudo script before the failed build", r.calls)
	}
}

// harnessLineREs are the only lines of the provisioning script that may name claude or
// codex: comments, pins, the install lines and the [ -x ] checks. None runs either binary.
var harnessLineREs = []*regexp.Regexp{
	regexp.MustCompile(`^#`),
	regexp.MustCompile(`^(CLAUDE_CODE|CODEX)_(VERSION|SHA256)=[A-Za-z0-9.-]+$`),
	regexp.MustCompile(`^echo "== harness CLIs \(installed, never run\)"$`),
	regexp.MustCompile(`^CLAUDE_CODE_DIR=/usr/local/lib/node_modules/@anthropic-ai/claude-code$`),
	regexp.MustCompile(`^CLAUDE_CODE_NATIVE_DIR=/usr/local/lib/node_modules/@anthropic-ai/claude-code/node_modules/@anthropic-ai/claude-code-linux-arm64$`),
	// claude_code_installed reads the files: versions, ELF magic, -x, cmp and readlink.
	regexp.MustCompile(`^claude_code_installed\(\) \{$`),
	regexp.MustCompile(`^\[ "\$\(version "\$CLAUDE_CODE_(NATIVE_)?DIR"\)" = "\$CLAUDE_CODE_VERSION" \] &&$`),
	regexp.MustCompile(`^elf "\$CLAUDE_CODE_NATIVE_DIR/claude" &&$`),
	regexp.MustCompile(`^\[ -x "\$CLAUDE_CODE_DIR/bin/claude\.exe" \] &&$`),
	regexp.MustCompile(`^cmp -s "\$CLAUDE_CODE_NATIVE_DIR/claude" "\$CLAUDE_CODE_DIR/bin/claude\.exe" &&$`),
	regexp.MustCompile(`^\[ "\$\(readlink -f /usr/local/bin/claude\)" = "\$CLAUDE_CODE_DIR/bin/claude\.exe" \]$`),
	regexp.MustCompile(`^if ! claude_code_installed; then$`),
	regexp.MustCompile(`^sudo npm install -g --prefix /usr/local --no-fund --no-audit --allow-scripts=@anthropic-ai/claude-code "@anthropic-ai/claude-code@\$CLAUDE_CODE_VERSION"$`),
	regexp.MustCompile(`^claude_code_installed \|\| \{ echo "claude-code's native binary isn't in place: \$CLAUDE_CODE_DIR/bin/claude\.exe" >&2; exit 1; \}$`),
	regexp.MustCompile(`^\[ -x /usr/local/bin/codex \] \|\| \{ echo "[^"$` + "`" + `]*" >&2; exit 1; \}$`),
	regexp.MustCompile(`^if \[ "\$\(cat /var/lib/factory/codex\.tar\.sha256 2>/dev/null \|\| true\)" != "\$CODEX_SHA256" \] \|\| \[ ! -x /usr/local/bin/codex \]; then$`),
	regexp.MustCompile(`^fetch "https://github\.com/openai/codex/releases/download/\$CODEX_VERSION/codex-aarch64-unknown-linux-musl\.tar\.gz" "\$CODEX_SHA256" "\$TMP/codex\.tar\.gz"$`),
	regexp.MustCompile(`^mkdir "\$TMP/codex"$`),
	regexp.MustCompile(`^tar -xzf "\$TMP/codex\.tar\.gz" -C "\$TMP/codex" --no-same-owner$`),
	regexp.MustCompile(`^sudo install -m 0755 "\$TMP/codex/codex-aarch64-unknown-linux-musl" /usr/local/bin/codex$`),
	regexp.MustCompile(`^echo "\$CODEX_SHA256" \| sudo tee /var/lib/factory/codex\.tar\.sha256 >/dev/null$`),
}

var harnessRE = regexp.MustCompile(`(?i)claude|codex`)

// harnessLines are the script's lines that name claude or codex outside harnessLineREs.
func harnessLines(script string) []string {
	var bad []string
	for l := range strings.Lines(script) {
		l = strings.TrimSpace(l)
		if !harnessRE.MatchString(l) {
			continue
		}
		if !slices.ContainsFunc(harnessLineREs, func(re *regexp.Regexp) bool { return re.MatchString(l) }) {
			bad = append(bad, l)
		}
	}
	return bad
}

// ghRE is gh run as a command word (a line that runs it), not gh_extension or gh-stack.
var ghRE = regexp.MustCompile(`(^|[\s;|&(])gh(\s|$)`)

// ghLines are the script's lines that run gh: every one but comments, the section banner and
// the apt install.
func ghLines(script string) []string {
	var bad []string
	for l := range strings.Lines(script) {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") || strings.HasPrefix(l, `echo "== `) || l == "apt_get install gh" {
			continue
		}
		if ghRE.MatchString(l) {
			bad = append(bad, l)
		}
	}
	return bad
}

func TestProvisionScript(t *testing.T) {
	if bad := harnessLines(string(provisionScript)); len(bad) > 0 {
		t.Errorf("provision/base.sh names claude or codex outside the install lines and existence checks:\n%s", strings.Join(bad, "\n"))
	}
	for _, l := range []string{"claude --version", "/usr/local/bin/codex --help", `[ -x /usr/local/bin/codex ] && codex login`, `"$(command -v claude)" -p hi`, "x=claude; $x", `"$CLAUDE_CODE_DIR/bin/claude.exe" --version`, `claude_code_installed && "$CLAUDE_CODE_NATIVE_DIR/claude" -p hi`} {
		if bad := harnessLines(string(provisionScript) + l + "\n"); len(bad) != 1 {
			t.Errorf("the check misses %q", l)
		}
	}
	for _, w := range []string{"set -euo pipefail", "KIND_VERSION=v0.33.0", "KUBECTL_VERSION=v1.37.1", "CODEX_SHA256=a7261e65f4a96d8f4ca65a069c6cf2dc2c33defc341e48078acefda9461fffe4", `main "$@" </dev/null`,
		"GH_PR_REVIEW_VERSION=v1.6.2", "GH_PR_REVIEW_SHA256=fea079f78e7224200d23de847b6210134ccec1a4d2dcff96ca8ca4ce1060fdde",
		"GH_STACK_VERSION=v0.2.0", "GH_STACK_SHA256=d29684746f337a580667605b34877e5624792bcc390e61473a050bd4839557bf",
		"ispinned: true",
		// claude-code's postinstall only places the native binary (see the script's comment).
		"--allow-scripts=@anthropic-ai/claude-code",
		"DOCKER_KEY_SHA256=1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570",
		"GH_KEY_SHA256=6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b",
		"sudo systemctl mask apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service"} {
		if !bytes.Contains(provisionScript, []byte(w)) {
			t.Errorf("the script lacks %q", w)
		}
	}
	// every download goes through fetch's sha256 check, the apt keys included.
	for l := range strings.Lines(string(provisionScript)) {
		if strings.Contains(l, "sudo curl") {
			t.Errorf("an unchecked download: %s", l)
		}
	}
	// the apt timers are off before the first apt-get.
	if i, j := bytes.Index(provisionScript, []byte("\tno_apt_timers\n")), bytes.Index(provisionScript, []byte("\tsection_apt\n")); i < 0 || j < i {
		t.Error("main doesn't run no_apt_timers before section_apt")
	}
	// the base build has no gh login: the script never runs gh, only installs it.
	if bad := ghLines(string(provisionScript)); len(bad) > 0 {
		t.Errorf("provision/base.sh runs gh:\n%s", strings.Join(bad, "\n"))
	}
	for _, l := range []string{"gh extension install x/y", "sudo -u admin gh auth status", "x=$(gh extension list)"} {
		if bad := ghLines(string(provisionScript) + l + "\n"); len(bad) != 1 {
			t.Errorf("the gh check misses %q", l)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: skipping bash -n")
	}
	p := filepath.Join(t.TempDir(), "base.sh")
	if err := os.WriteFile(p, provisionScript, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// TestExecRunnerProcesses runs real, harmless processes: Start's Stop ends a sleep, and
// RunInput feeds stdin.
func TestExecRunnerProcesses(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	p, err := ExecRunner{}.Start(context.Background(), io.Discard, "sleep", "60")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p.Stop()
	p.Stop() // a second Stop does nothing
	select {
	case <-p.Exited():
	default:
		t.Fatal("Stop returned before the process exited")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Stop took %s", time.Since(start))
	}
	var out bytes.Buffer
	if err := (ExecRunner{}).RunInput(context.Background(), strings.NewReader("hello"), &out, "cat"); err != nil || out.String() != "hello" {
		t.Errorf("RunInput: %v %q", err, out.String())
	}
}
