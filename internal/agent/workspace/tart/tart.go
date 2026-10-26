// Package tart is the workspace driver for macOS: it drives the tart CLI (P1 §1,
// design/workstreams/agent/LLD/supervise.md). Each workspace is a clone of the warm base, run
// on Softnet with every address blocked but the Mac's (--net-softnet-allow=@host), so its only
// route out is the gateway on its bridge address.
package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/workspace"
	"github.com/compliance-framework/factory/internal/agent/workspace/vmexec"
)

// The runner seam is vmexec's, which both drivers share.
type (
	// Cmd is one tart invocation.
	Cmd = vmexec.Cmd
	// Runner runs tart. Tests use a fake; nothing in them runs the real tart.
	Runner = vmexec.Runner
	// ExecRunner runs real programs.
	ExecRunner = vmexec.ExecRunner
)

// The scripts tart exec runs in the VM are vmexec's; tart's fake answers by matching these.
const (
	putScript    = vmexec.PutScript
	signalScript = vmexec.SignalScript
)

// MaxGet bounds Get's compressed stream by default; host.Extract bounds what it unpacks to.
const MaxGet = 64 << 20

// Config is the driver's setup; every field but Bin has a default.
type Config struct {
	// Bin is the tart to run: host.Env.TartBin, the one host check accepts. Default "tart".
	Bin string
	// WarmBase is the VM Create clones: default host.WarmBaseVM.
	WarmBase string
	// Path is the PATH tart run gets after Softnet's directory (host.SoftnetPath's), so Tart
	// runs host init's Softnet; default the process's PATH.
	Path   string
	Runner Runner
	// Log gets tart run's output; default io.Discard.
	Log io.Writer
	// Boot bounds Start's wait for tart exec to answer (3 minutes), polled every Poll (1 s).
	Boot, Poll time.Duration
	// Grace is how long Exec waits after the remote SIGTERM before SIGKILL (45 s: factory-agent
	// run's down gets at most 30 s after a SIGTERM).
	Grace time.Duration
	// MaxGet bounds Get's stream: default MaxGet.
	MaxGet int64
}

// Driver is the tart workspace driver. It's safe for concurrent use on different workspaces.
type Driver struct {
	c Config
	// x runs commands in a workspace through tart exec [-i] <name>.
	x *vmexec.Remote
	// log is Config.Log, shared by every workspace's tart run.
	log *host.LockedWriter

	mu    sync.Mutex
	procs map[string]host.Process // the tart run of each workspace this driver started
}

var _ workspace.Driver = (*Driver)(nil)

// New returns a driver.
func New(c Config) *Driver {
	if c.Bin == "" {
		c.Bin = "tart"
	}
	if c.WarmBase == "" {
		c.WarmBase = host.WarmBaseVM
	}
	if c.Path == "" {
		c.Path = os.Getenv("PATH")
	}
	if c.Runner == nil {
		c.Runner = ExecRunner{}
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	for _, d := range []struct {
		p *time.Duration
		v time.Duration
	}{{&c.Boot, 3 * time.Minute}, {&c.Poll, time.Second}, {&c.Grace, 45 * time.Second}} {
		if *d.p == 0 {
			*d.p = d.v
		}
	}
	if c.MaxGet == 0 {
		c.MaxGet = MaxGet
	}
	x := vmexec.New(vmexec.Config{Runner: c.Runner, Label: "tart exec", Grace: c.Grace, Prefix: func(name string, stdin bool) []string {
		if stdin {
			return []string{c.Bin, "exec", "-i", name}
		}
		return []string{c.Bin, "exec", name}
	}})
	return &Driver{c: c, x: x, log: &host.LockedWriter{W: c.Log}, procs: map[string]host.Process{}}
}

// tart runs tart with args and returns its stdout; a failure carries stderr's first line.
func (d *Driver) tart(ctx context.Context, args ...string) (string, error) {
	return vmexec.Output(ctx, d.c.Runner, d.c.Bin, args...)
}

// Create implements workspace.Driver: tart clone <warm base> <name>, then tart set.
func (d *Driver) Create(ctx context.Context, name string, spec workspace.Spec) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	spec = spec.WithDefaults()
	if spec.CPU < 1 || spec.MemoryMiB < 1 {
		return fmt.Errorf("%s: invalid size %d CPUs, %d MiB", name, spec.CPU, spec.MemoryMiB)
	}
	if _, err := d.tart(ctx, "clone", d.c.WarmBase, name); err != nil {
		return fmt.Errorf("%s: tart clone %s: %w", name, d.c.WarmBase, err)
	}
	if _, err := d.tart(ctx, "set", name, "--cpu", strconv.Itoa(spec.CPU), "--memory", strconv.Itoa(spec.MemoryMiB)); err != nil {
		err = fmt.Errorf("%s: tart set: %w", name, err)
		cctx, cancel := vmexec.CleanupCtx(ctx)
		defer cancel()
		if _, derr := d.tart(cctx, "delete", name); derr != nil {
			err = errors.Join(err, fmt.Errorf("deleting %s: %w", name, derr))
		}
		return err
	}
	return nil
}

// Start implements workspace.Driver: tart run in the background on Softnet, with Softnet's
// directory first on PATH; then it polls tart exec true and reads the VM's routes.
func (d *Driver) Start(ctx context.Context, name string) (workspace.Network, error) {
	if err := workspace.ValidName(name); err != nil {
		return workspace.Network{}, err
	}
	d.mu.Lock()
	if _, ok := d.procs[name]; ok {
		d.mu.Unlock()
		return workspace.Network{}, fmt.Errorf("%s is already started", name)
	}
	// an empty element would be the current directory.
	pathEnv := "PATH=" + filepath.Dir(host.SoftnetPath)
	if d.c.Path != "" {
		pathEnv += ":" + d.c.Path
	}
	p, err := d.c.Runner.Start(Cmd{
		Name: d.c.Bin,
		Args: []string{"run", name, "--no-graphics", "--net-softnet-block=0.0.0.0/0", "--net-softnet-allow=@host"},
		Env:  []string{pathEnv},
		// tart run's output arrives from its own goroutines, and every VM's goes to one Log.
		Stdout: d.log, Stderr: d.log,
	})
	if err != nil {
		d.mu.Unlock()
		return workspace.Network{}, fmt.Errorf("%s: tart run: %w", name, err)
	}
	d.procs[name] = p
	d.mu.Unlock()
	nw, err := d.boot(ctx, name, p)
	if err != nil {
		d.reap(name)
		return workspace.Network{}, err
	}
	return nw, nil
}

func (d *Driver) boot(ctx context.Context, name string, p host.Process) (workspace.Network, error) {
	wctx, cancel := context.WithTimeout(ctx, d.c.Boot)
	defer cancel()
	for {
		if _, err := d.x.Output(wctx, name, "true"); err == nil {
			break
		}
		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return workspace.Network{}, err
			}
			return workspace.Network{}, fmt.Errorf("%s: timed out after %s waiting for its guest agent", name, d.c.Boot)
		case <-p.Exited():
			why := p.Err()
			if why == nil {
				why = errors.New("exit status 0")
			}
			return workspace.Network{}, fmt.Errorf("%s: tart run exited before its guest agent answered: %w", name, why)
		case <-time.After(d.c.Poll):
		}
	}
	return d.x.Network(ctx, name)
}

// Put implements workspace.Driver: a tar stream of srcDir (directories and regular files
// only) into tart exec -i … sudo sh -c putScript.
func (d *Driver) Put(ctx context.Context, name, srcDir, dstDir string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	return d.x.Put(ctx, name, srcDir, dstDir)
}

// Get implements workspace.Driver: tart exec … sudo tar -czf - -C <srcDir> . (or the named
// files) into a temp file next to dstDir (at most MaxGet bytes), then host.Extract into dstDir.
// A failure removes dstDir.
func (d *Driver) Get(ctx context.Context, name, srcDir, dstDir string, files ...string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	return d.x.Get(ctx, name, srcDir, dstDir, d.c.MaxGet, files...)
}

// Exec implements workspace.Driver: tart exec [-i] <name> sh -c vmexec.ExecScript <pid file> argv….
// When ctx is done it signals the remote process through another tart exec (TERM, then KILL
// after Grace) and stops the local tart exec once it hasn't exited.
func (d *Driver) Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	return d.x.Exec(ctx, name, argv, stdin, stdout, stderr, nil)
}

// exitError maps a non-zero exit to *workspace.ExitError.
func exitError(name string, err error) error {
	return vmexec.ExitError(name, "tart exec", err)
}

// stopVM runs tart stop; a VM that has stopped meanwhile ("is not running") is fine.
func (d *Driver) stopVM(ctx context.Context, name string) error {
	if _, err := d.tart(ctx, "stop", name); err != nil && !strings.Contains(err.Error(), "is not running") {
		return fmt.Errorf("%s: tart stop: %w", name, err)
	}
	return nil
}

// reap stops the workspace's tart run, if this driver started one, and forgets it.
func (d *Driver) reap(name string) {
	d.mu.Lock()
	p := d.procs[name]
	delete(d.procs, name)
	d.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}

// Stop implements workspace.Driver: tart stop, then its tart run is reaped. It runs after
// ctx is done too.
func (d *Driver) Stop(ctx context.Context, name string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	cctx, cancel := vmexec.CleanupCtx(ctx)
	defer cancel()
	err := d.stopVM(cctx, name)
	d.reap(name)
	return err
}

// Destroy implements workspace.Driver: tart stop when it's running, then tart delete. It
// runs after ctx is done too, and its tart run is reaped on every path: its SIGTERM ends the
// VM when tart stop never ran.
func (d *Driver) Destroy(ctx context.Context, name string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	defer d.reap(name)
	cctx, cancel := vmexec.CleanupCtx(ctx)
	defer cancel()
	vms, err := d.vms(cctx)
	if err != nil {
		return err
	}
	state, ok := vms[name]
	if ok && state != "stopped" {
		if err := d.stopVM(cctx, name); err != nil {
			return err
		}
	}
	d.reap(name)
	if !ok {
		return nil
	}
	if _, err := d.tart(cctx, "delete", name); err != nil {
		return fmt.Errorf("%s: tart delete: %w", name, err)
	}
	return nil
}

// List implements workspace.Driver: tart list's local VMs with a workspace name.
func (d *Driver) List(ctx context.Context) ([]string, error) {
	vms, err := d.vms(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for n := range vms {
		if workspace.ValidName(n) == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// vms is tart list's local VMs: name -> state.
func (d *Driver) vms(ctx context.Context) (map[string]string, error) {
	out, err := d.tart(ctx, "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	var list []struct{ Name, State, Source string }
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	m := map[string]string{}
	for _, v := range list {
		if v.Source == "local" {
			m[v.Name] = v.State
		}
	}
	return m, nil
}
