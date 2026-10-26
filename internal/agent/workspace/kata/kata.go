// Package kata is the workspace driver for Linux: each workspace is a Kata microVM made and run
// by the root helper (design/workstreams/agent/LLD/workspace-kata.md#the-driver-internalagentworkspacekata),
// which this driver runs as sudo -n kata-helper <verb>. Commands in a workspace go through
// kata-helper exec, with vmexec's tar streams and pid-file cancel, as tart's go through tart exec.
package kata

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

	"github.com/compliance-framework/factory/internal/agent/katahelper"
	"github.com/compliance-framework/factory/internal/agent/workspace"
	"github.com/compliance-framework/factory/internal/agent/workspace/vmexec"
)

// The agent binary's place in a workspace: Start puts it in AgentDir and links AgentLink to it,
// where workspace.AgentCommand finds it on PATH.
const (
	AgentDir  = "/var/lib/factory/bin"
	AgentLink = "/usr/local/bin/factory-agent"
)

// Config is the driver's setup; every field but Image has a default.
type Config struct {
	// Helper is the root helper: default katahelper.HelperPath, the path the sudoers rule names.
	Helper string
	// Sudo runs the helper as root without a prompt: default sudo -n.
	Sudo   []string
	Runner vmexec.Runner
	// Image is the workspace image, factory-agent-base:<version> (host init's katabase.json).
	Image string
	// Warm is whether host init's warm disk exists (katawarm.json): Create copies it, else it
	// makes an empty disk.
	Warm bool
	// AgentBinary is the factory-agent Start puts in each workspace: default this executable
	// (the host and the guest are both linux/amd64).
	AgentBinary string
	// Boot bounds Start's wait for systemd in the workspace (3 minutes), polled every Poll (1 s).
	Boot, Poll time.Duration
	// Grace is how long Exec waits after the remote SIGTERM before SIGKILL (45 s, as tart's).
	Grace time.Duration
	// MaxGet bounds Get's stream: default vmexec.MaxGet.
	MaxGet int64
}

// Driver is the kata workspace driver. It keeps no state per workspace, so it's safe for
// concurrent use on different workspaces.
type Driver struct {
	c Config
	// x runs commands in a workspace through kata-helper exec [-i] <name>.
	x *vmexec.Remote
	// agentErr is why AgentBinary has no default.
	agentErr error
}

var _ workspace.Driver = (*Driver)(nil)

// New returns a driver.
func New(c Config) *Driver {
	if c.Helper == "" {
		c.Helper = katahelper.HelperPath
	}
	if len(c.Sudo) == 0 {
		c.Sudo = []string{"sudo", "-n"}
	}
	if c.Runner == nil {
		c.Runner = vmexec.ExecRunner{}
	}
	var agentErr error
	if c.AgentBinary == "" {
		c.AgentBinary, agentErr = os.Executable()
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
		c.MaxGet = vmexec.MaxGet
	}
	helper := append(slices.Clone(c.Sudo), c.Helper)
	x := vmexec.New(vmexec.Config{Runner: c.Runner, Label: "kata-helper exec", Grace: c.Grace, Prefix: func(name string, stdin bool) []string {
		p := append(slices.Clone(helper), "exec")
		if stdin {
			p = append(p, "-i")
		}
		return append(p, name)
	}})
	return &Driver{c: c, x: x, agentErr: agentErr}
}

// helper runs kata-helper <verb> args and returns its stdout; a failure carries its stderr line.
func (d *Driver) helper(ctx context.Context, verb string, args ...string) (string, error) {
	argv := append(append(slices.Clone(d.c.Sudo[1:]), d.c.Helper, verb), args...)
	out, err := vmexec.Output(ctx, d.c.Runner, d.c.Sudo[0], argv...)
	if err != nil {
		return out, fmt.Errorf("kata-helper %s: %w", verb, err)
	}
	return out, nil
}

// Create implements workspace.Driver: kata-helper create <name> <cpu-1> <MiB> warm|empty
// <image>. Kata adds a vCPU of its own to the container's (Q8 H06).
func (d *Driver) Create(ctx context.Context, name string, spec workspace.Spec) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	if d.c.Image == "" {
		return errors.New("kata: no workspace image configured")
	}
	spec = spec.WithDefaults()
	if spec.CPU < 1 || spec.MemoryMiB < 1 {
		return fmt.Errorf("%s: invalid size %d CPUs, %d MiB", name, spec.CPU, spec.MemoryMiB)
	}
	disk := "empty"
	if d.c.Warm {
		disk = "warm"
	}
	_, err := d.helper(ctx, "create", name, strconv.Itoa(max(spec.CPU-1, 1)), strconv.Itoa(spec.MemoryMiB), disk, d.c.Image)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Start implements workspace.Driver: kata-helper start, then it waits for systemd (which also
// proves exec works after its boot), reads the network and checks it against the slot's, and
// puts the agent binary in. A failure destroys nothing: the caller does.
func (d *Driver) Start(ctx context.Context, name string) (workspace.Network, error) {
	if err := workspace.ValidName(name); err != nil {
		return workspace.Network{}, err
	}
	if d.agentErr != nil {
		return workspace.Network{}, fmt.Errorf("kata: the agent binary: %w", d.agentErr)
	}
	if _, err := d.helper(ctx, "start", name); err != nil {
		return workspace.Network{}, fmt.Errorf("%s: %w", name, err)
	}
	if err := d.boot(ctx, name); err != nil {
		return workspace.Network{}, err
	}
	nw, err := d.x.Network(ctx, name)
	if err != nil {
		return workspace.Network{}, err
	}
	out, err := d.helper(ctx, "inspect", name)
	if err != nil {
		return workspace.Network{}, fmt.Errorf("%s: %w", name, err)
	}
	var slot katahelper.Info
	if err := json.Unmarshal([]byte(out), &slot); err != nil {
		return workspace.Network{}, fmt.Errorf("%s: kata-helper inspect: %w", name, err)
	}
	if nw.VM != slot.VM || nw.Gateway != slot.Gateway {
		return workspace.Network{}, fmt.Errorf("%s: its routes say VM %s, gateway %s; its slot %d is %s, %s", name, nw.VM, nw.Gateway, slot.Slot, slot.VM, slot.Gateway)
	}
	if err := d.putAgent(ctx, name); err != nil {
		return workspace.Network{}, err
	}
	return nw, nil
}

// boot polls test -S /run/systemd/private until it succeeds or Boot passes.
func (d *Driver) boot(ctx context.Context, name string) error {
	wctx, cancel := context.WithTimeout(ctx, d.c.Boot)
	defer cancel()
	for {
		if _, err := d.x.Output(wctx, name, "test", "-S", "/run/systemd/private"); err == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("%s: timed out after %s waiting for its systemd", name, d.c.Boot)
		case <-time.After(d.c.Poll):
		}
	}
}

// putAgent puts AgentBinary, alone in a temp dir, at AgentDir and links AgentLink to it.
func (d *Driver) putAgent(ctx context.Context, name string) error {
	dir, err := os.MkdirTemp("", "factory-agent-bin-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := copyFile(d.c.AgentBinary, filepath.Join(dir, "factory-agent")); err != nil {
		return fmt.Errorf("kata: the agent binary: %w", err)
	}
	if err := d.x.Put(ctx, name, dir, AgentDir); err != nil {
		return err
	}
	if _, err := d.x.Output(ctx, name, "ln", "-sfn", AgentDir+"/factory-agent", AgentLink); err != nil {
		return fmt.Errorf("%s: linking %s: %w", name, AgentLink, err)
	}
	return nil
}

// copyFile copies the regular file src to dst, executable.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// Put implements workspace.Driver: vmexec's tar stream through kata-helper exec -i.
func (d *Driver) Put(ctx context.Context, name, srcDir, dstDir string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	return d.x.Put(ctx, name, srcDir, dstDir)
}

// Get implements workspace.Driver: vmexec's, at most MaxGet bytes.
func (d *Driver) Get(ctx context.Context, name, srcDir, dstDir string, files ...string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	return d.x.Get(ctx, name, srcDir, dstDir, d.c.MaxGet, files...)
}

// Exec implements workspace.Driver: vmexec's, through kata-helper exec [-i]. The helper exits
// with the remote command's status, or katahelper.ExecFailed with its own error as the last
// stderr line: only that pair is an exec failure, so a remote command's own 125 stays its
// status.
func (d *Driver) Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	tail := &lastLine{}
	return d.x.Exec(ctx, name, argv, stdin, stdout, io.MultiWriter(stderr, tail), func(err error) error {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) && ec.ExitCode() == katahelper.ExecFailed {
			if l := tail.String(); strings.HasPrefix(l, katahelper.ExecFailedPrefix) {
				return fmt.Errorf("%s: kata-helper exec: %s", name, strings.TrimPrefix(l, katahelper.ExecFailedPrefix))
			}
		}
		return vmexec.ExitError(name, "kata-helper exec", err)
	})
}

// lastLine keeps the last line written to it, at most 1 KiB of it. Exec's status reads it only
// once the command has exited, after its writes.
type lastLine struct {
	mu  sync.Mutex
	cur []byte
	end []byte // the last complete line
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if len(l.cur) > 0 {
				l.end, l.cur = l.cur, nil
			}
			continue
		}
		if len(l.cur) < 1<<10 {
			l.cur = append(l.cur, b)
		}
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.cur) > 0 {
		return string(l.cur)
	}
	return string(l.end)
}

// Stop implements workspace.Driver: kata-helper stop (systemd's halt). It runs after ctx is
// done too.
func (d *Driver) Stop(ctx context.Context, name string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	cctx, cancel := vmexec.CleanupCtx(ctx)
	defer cancel()
	if _, err := d.helper(cctx, "stop", name); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Destroy implements workspace.Driver: kata-helper rm, which stops and removes the workspace
// and frees its slot; a missing one is no error. It runs after ctx is done too, with the
// helper's own bounds (up to about four minutes for a hung VM).
func (d *Driver) Destroy(ctx context.Context, name string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if _, err := d.helper(cctx, "rm", name); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// List implements workspace.Driver: kata-helper list, which names only workspaces.
func (d *Driver) List(ctx context.Context) ([]string, error) {
	out, err := d.helper(ctx, "list")
	if err != nil {
		return nil, err
	}
	var names []string
	for l := range strings.Lines(out) {
		if n := strings.TrimSpace(l); workspace.ValidName(n) == nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names, nil
}
