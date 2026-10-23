// Package tart is the workspace driver for macOS: it drives the tart CLI (P1 §1,
// design/workstreams/agent/LLD/supervise.md). Each workspace is a clone of the warm base, run
// on Softnet with every address blocked but the Mac's (--net-softnet-allow=@host), so its only
// route out is the gateway on its bridge address.
package tart

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/workspace"
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
	c   Config
	seq atomic.Int64
	// log is Config.Log, shared by every workspace's tart run.
	log *lockedWriter

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
	return &Driver{c: c, log: &lockedWriter{w: c.Log}, procs: map[string]host.Process{}}
}

// tart runs tart with args and returns its stdout; a failure carries stderr's first line.
func (d *Driver) tart(ctx context.Context, args ...string) (string, error) {
	// a VM answers some of these: its output is capped.
	out, errb := &limitedBuffer{n: 4 << 20}, &limitedBuffer{n: 8 << 10}
	if err := d.c.Runner.Run(ctx, Cmd{Name: d.c.Bin, Args: args, Stdout: out, Stderr: errb}); err != nil {
		return out.String(), withStderr(err, errb.String())
	}
	return out.String(), nil
}

// withStderr adds stderr's first line, at most 256 bytes of it: a VM may write it.
func withStderr(err error, stderr string) error {
	l, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if len(l) > 256 {
		l = l[:256] + "…"
	}
	if l != "" {
		return fmt.Errorf("%w: %q", err, l)
	}
	return err
}

// cleanupCtx is a context for cleanup that runs after ctx is done too.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
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
		cctx, cancel := cleanupCtx(ctx)
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
		if _, err := d.tart(wctx, "exec", name, "true"); err == nil {
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
	return d.network(ctx, name)
}

// network reads the VM's default route (via: the Mac's address on its Softnet /29) and the
// address the VM sends from to it (src: its leased address).
func (d *Driver) network(ctx context.Context, name string) (workspace.Network, error) {
	out, err := d.tart(ctx, "exec", name, "ip", "-4", "route", "show", "default")
	if err != nil {
		return workspace.Network{}, fmt.Errorf("%s: reading its default route: %w", name, err)
	}
	var gws []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "default" {
			gws = append(gws, field(f, "via"))
		}
	}
	if len(gws) != 1 {
		return workspace.Network{}, fmt.Errorf("%s: want one default route, got %q", name, out)
	}
	gw, err := addr(gws[0])
	if err != nil {
		return workspace.Network{}, fmt.Errorf("%s: its default route %q: %w", name, out, err)
	}
	out, err = d.tart(ctx, "exec", name, "ip", "-4", "route", "get", gw.String())
	if err != nil {
		return workspace.Network{}, fmt.Errorf("%s: reading its address: %w", name, err)
	}
	l, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	vm, err := addr(field(strings.Fields(l), "src"))
	if err != nil {
		return workspace.Network{}, fmt.Errorf("%s: its route to %s %q: %w", name, gw, out, err)
	}
	if vm == gw {
		return workspace.Network{}, fmt.Errorf("%s: its address is its gateway's, %s", name, vm)
	}
	return workspace.Network{VM: vm, Gateway: gw}, nil
}

// field is the word after key in f, or "".
func field(f []string, key string) string {
	if i := slices.Index(f, key); i >= 0 && i+1 < len(f) {
		return f[i+1]
	}
	return ""
}

func addr(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	switch {
	case err != nil:
		return netip.Addr{}, err
	case !a.Is4() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast():
		return netip.Addr{}, fmt.Errorf("%s isn't a unicast IPv4 address", a)
	}
	return a, nil
}

// validDir checks a VM path: absolute, clean, not /.
func validDir(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("VM path %q isn't a clean absolute path below /", p)
	}
	return nil
}

// validFile accepts a plain file name for Get: not empty, ".", "..", an option or a path.
func validFile(f string) error {
	if f == "" || f == "." || f == ".." || strings.HasPrefix(f, "-") || strings.ContainsAny(f, "/\\\x00") {
		return fmt.Errorf("VM file name %q isn't a plain name", f)
	}
	return nil
}

// putScript, run as root with $0 the destination, replaces it with the tar stream on stdin,
// root's and read-only: a+rX,a-w makes files 0444 and directories and executables 0555.
const putScript = `set -eu; rm -rf "$0"; mkdir -p "$0"; tar -xf - -C "$0" --no-same-owner; chown -R root:root "$0"; chmod -R a+rX,a-w "$0"`

// Put implements workspace.Driver: a tar stream of srcDir (directories and regular files
// only) into tart exec -i … sudo sh -c putScript.
func (d *Driver) Put(ctx context.Context, name, srcDir, dstDir string) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	if err := validDir(dstDir); err != nil {
		return err
	}
	if fi, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("put: %w", err)
	} else if !fi.IsDir() {
		return fmt.Errorf("put %s: not a directory", srcDir)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, srcDir)) }()
	errb := &limitedBuffer{n: 8 << 10}
	err := d.c.Runner.Run(ctx, Cmd{
		Name: d.c.Bin, Args: []string{"exec", "-i", name, "sudo", "-n", "sh", "-c", putScript, dstDir},
		Stdin: pr, Stdout: errb, Stderr: errb,
	})
	// the writer may still be blocked on a pipe nobody reads.
	_ = pr.CloseWithError(errors.New("tart exec exited"))
	if err != nil {
		return fmt.Errorf("%s: putting %s at %s: %w", name, srcDir, dstDir, withStderr(err, errb.String()))
	}
	return nil
}

// writeTar writes dir's tree as a tar stream, owned by root.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		h := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Uname: "root", Gname: "root"}
		switch {
		case fi.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
			return tw.WriteHeader(h)
		case fi.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, fi.Size()
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.CopyN(tw, f, fi.Size())
			return err
		}
		return fmt.Errorf("%s: only directories and regular files are copied in", p)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Get implements workspace.Driver: tart exec … sudo tar -czf - -C <srcDir> . (or the named
// files, with GNU tar's --ignore-failed-read so a missing one is only a warning) into a temp
// file next to dstDir (at most MaxGet bytes), then host.Extract into dstDir. A failure removes
// dstDir.
func (d *Driver) Get(ctx context.Context, name, srcDir, dstDir string, files ...string) (err error) {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	if err := validDir(srcDir); err != nil {
		return err
	}
	args := []string{"exec", name, "sudo", "-n", "tar", "-czf", "-", "-C", srcDir, "."}
	if len(files) > 0 {
		for _, f := range files {
			if err := validFile(f); err != nil {
				return err
			}
		}
		args = append([]string{"exec", name, "sudo", "-n", "tar", "-czf", "-", "--ignore-failed-read", "-C", srcDir}, files...)
	}
	if err := os.Mkdir(dstDir, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dstDir)
		}
	}()
	f, err := os.CreateTemp(filepath.Dir(dstDir), ".get-*.tgz")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	errb := &limitedBuffer{n: 8 << 10}
	cw := &capWriter{w: f, n: d.c.MaxGet}
	rerr := d.c.Runner.Run(ctx, Cmd{
		Name: d.c.Bin, Args: args,
		Stdout: cw, Stderr: errb,
	})
	switch {
	case cw.over:
		return fmt.Errorf("%s: getting %s: more than %d bytes", name, srcDir, d.c.MaxGet)
	case rerr != nil:
		return fmt.Errorf("%s: getting %s: %w", name, srcDir, withStderr(rerr, errb.String()))
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := host.Extract(f.Name(), dstDir); err != nil {
		return fmt.Errorf("%s: getting %s: %w", name, srcDir, err)
	}
	return nil
}

// capWriter writes at most n bytes, then fails.
type capWriter struct {
	w    io.Writer
	n    int64
	over bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.n {
		c.over = true
		return 0, errors.New("over the cap")
	}
	c.n -= int64(len(p))
	return c.w.Write(p)
}

// limitedBuffer keeps the first n bytes and drops the rest.
type limitedBuffer struct {
	bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if r := l.n - l.Len(); r > 0 {
		l.Buffer.Write(p[:min(r, len(p))])
	}
	return len(p), nil
}

// execScript, with $0 the pid file and "$@" the command, records its pid and becomes the
// command, so a cancel can signal it: tart exec doesn't forward signals.
const execScript = `echo $$ > "$0" && exec "$@"`

// signalScript, with $0 the pid file and $1 the signal, signals the process it names.
const signalScript = `test -s "$0" && exec sudo -n kill -s "$1" "$(cat "$0")"`

// Exec implements workspace.Driver: tart exec [-i] <name> sh -c execScript <pid file> argv….
// When ctx is done it signals the remote process through another tart exec (TERM, then KILL
// after Grace) and stops the local tart exec once it hasn't exited.
func (d *Driver) Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := workspace.ValidName(name); err != nil {
		return err
	}
	if len(argv) == 0 {
		return errors.New("exec: no command")
	}
	pid := fmt.Sprintf("/tmp/factory-exec-%d.pid", d.seq.Add(1))
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(append(args, name, "sh", "-c", execScript, pid), argv...)
	p, err := d.c.Runner.Start(Cmd{Name: d.c.Bin, Args: args, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if err != nil {
		return fmt.Errorf("%s: tart exec: %w", name, err)
	}
	select {
	case <-p.Exited():
		return exitError(name, p.Err())
	case <-ctx.Done():
	}
	d.signal(ctx, name, pid, "TERM")
	select {
	case <-p.Exited():
	case <-time.After(d.c.Grace):
		d.signal(ctx, name, pid, "KILL")
		p.Stop()
	}
	return fmt.Errorf("%s: %s: %w", name, argv[0], ctx.Err())
}

func (d *Driver) signal(ctx context.Context, name, pid, sig string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	// a failure (the process hadn't written its pid yet, or has exited) leaves the kill after
	// Grace, and then Destroy.
	_, _ = d.tart(cctx, "exec", name, "sh", "-c", signalScript, pid, sig)
}

// exitError maps a non-zero exit to *workspace.ExitError.
func exitError(name string, err error) error {
	if err == nil {
		return nil
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) && ec.ExitCode() > 0 {
		return fmt.Errorf("%s: %w", name, &workspace.ExitError{Code: ec.ExitCode()})
	}
	return fmt.Errorf("%s: tart exec: %w", name, err)
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
	cctx, cancel := cleanupCtx(ctx)
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
	cctx, cancel := cleanupCtx(ctx)
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

// lockedWriter serialises writes.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
