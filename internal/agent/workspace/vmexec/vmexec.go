// Package vmexec is what the workspace drivers share (design/workstreams/agent/LLD/
// workspace-kata.md#the-driver-internalagentworkspacekata): running commands in a workspace
// through the driver's exec command, and over it Put's and Get's tar streams, Exec with its
// pid-file cancel, and the network read. A driver supplies only its exec prefix (tart exec [-i]
// <name>, or sudo -n kata-helper exec [-i] <name>) and its lifecycle verbs.
//
// Callers check workspace names (workspace.ValidName) before calling; this package checks the
// VM paths and file names, and treats everything the VM sends back as untrusted.
package vmexec

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// Config is a driver's exec command and its cancel grace.
type Config struct {
	Runner Runner
	// Prefix is the command that runs argv in workspace name, its binary first; stdin is set
	// when argv reads standard input (tart exec -i).
	Prefix func(name string, stdin bool) []string
	// Label names the exec command in errors: "tart exec".
	Label string
	// Grace is how long Exec waits after the remote SIGTERM before SIGKILL.
	Grace time.Duration
}

// Remote runs commands in a driver's workspaces. It's safe for concurrent use.
type Remote struct {
	c   Config
	seq atomic.Int64
}

// New returns a Remote; c's fields are all required.
func New(c Config) *Remote { return &Remote{c: c} }

// cmd is argv run in name through the prefix.
func (r *Remote) cmd(name string, stdin bool, argv ...string) Cmd {
	p := r.c.Prefix(name, stdin)
	return Cmd{Name: p[0], Args: append(slices.Clone(p[1:]), argv...)}
}

// Output runs bin with args and returns its stdout; a failure carries stderr's first line.
func Output(ctx context.Context, r Runner, bin string, args ...string) (string, error) {
	// a VM answers some of these: its output is capped.
	out, errb := &limitedBuffer{n: 4 << 20}, &limitedBuffer{n: 8 << 10}
	if err := r.Run(ctx, Cmd{Name: bin, Args: args, Stdout: out, Stderr: errb}); err != nil {
		return out.String(), withStderr(err, errb.String())
	}
	return out.String(), nil
}

// Output runs argv in name and returns its stdout, as the package's Output.
func (r *Remote) Output(ctx context.Context, name string, argv ...string) (string, error) {
	c := r.cmd(name, false, argv...)
	return Output(ctx, r.c.Runner, c.Name, c.Args...)
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

// CleanupCtx is a context for cleanup that runs after ctx is done too.
func CleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}

// Network reads the VM's default route (via: the host's address on the VM's network) and the
// address the VM sends from to it (src: its own).
func (r *Remote) Network(ctx context.Context, name string) (workspace.Network, error) {
	out, err := r.Output(ctx, name, "ip", "-4", "route", "show", "default")
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
	out, err = r.Output(ctx, name, "ip", "-4", "route", "get", gw.String())
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

// PutScript, run as root with $0 the destination, replaces it with the tar stream on stdin,
// root's and read-only: a+rX,a-w makes files 0444 and directories and executables 0555.
const PutScript = `set -eu; rm -rf "$0"; mkdir -p "$0"; tar -xf - -C "$0" --no-same-owner; chown -R root:root "$0"; chmod -R a+rX,a-w "$0"`

// Put implements workspace.Driver's Put: a tar stream of srcDir (directories and regular
// files only) into <prefix -i> sudo sh -c PutScript.
func (r *Remote) Put(ctx context.Context, name, srcDir, dstDir string) error {
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
	c := r.cmd(name, true, "sudo", "-n", "sh", "-c", PutScript, dstDir)
	c.Stdin, c.Stdout, c.Stderr = pr, errb, errb
	err := r.c.Runner.Run(ctx, c)
	// the writer may still be blocked on a pipe nobody reads.
	_ = pr.CloseWithError(errors.New(r.c.Label + " exited"))
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

// Get implements workspace.Driver's Get: <prefix> sudo tar -czf - -C <srcDir> . (or the named
// files, with GNU tar's --ignore-failed-read so a missing one is only a warning) into a temp
// file next to dstDir (at most maxBytes), then host.Extract into dstDir. A failure removes
// dstDir.
func (r *Remote) Get(ctx context.Context, name, srcDir, dstDir string, maxBytes int64, files ...string) (err error) {
	if err := validDir(srcDir); err != nil {
		return err
	}
	argv := []string{"sudo", "-n", "tar", "-czf", "-", "-C", srcDir, "."}
	if len(files) > 0 {
		for _, f := range files {
			if err := validFile(f); err != nil {
				return err
			}
		}
		argv = append([]string{"sudo", "-n", "tar", "-czf", "-", "--ignore-failed-read", "-C", srcDir}, files...)
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
	cw := &capWriter{w: f, n: maxBytes}
	c := r.cmd(name, false, argv...)
	c.Stdout, c.Stderr = cw, errb
	rerr := r.c.Runner.Run(ctx, c)
	switch {
	case cw.over:
		return fmt.Errorf("%s: getting %s: more than %d bytes", name, srcDir, maxBytes)
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

// ExecScript, with $0 the pid file and "$@" the command, records its pid and becomes the
// command, so a cancel can signal it: the exec commands don't forward signals.
const ExecScript = `echo $$ > "$0" && exec "$@"`

// SignalScript, with $0 the pid file and $1 the signal, signals the process it names.
const SignalScript = `test -s "$0" && exec sudo -n kill -s "$1" "$(cat "$0")"`

// Exec implements workspace.Driver's Exec: <prefix [-i]> sh -c ExecScript <pid file> argv….
// When ctx is done it signals the remote process through another exec (TERM, then KILL after
// Grace) and stops the local exec command once it hasn't exited. status maps the exec
// command's exit (nil for success) to Exec's error.
func (r *Remote) Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer, status func(error) error) error {
	if len(argv) == 0 {
		return errors.New("exec: no command")
	}
	pid := fmt.Sprintf("/tmp/factory-exec-%d.pid", r.seq.Add(1))
	c := r.cmd(name, stdin != nil, append([]string{"sh", "-c", ExecScript, pid}, argv...)...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	p, err := r.c.Runner.Start(c)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", name, r.c.Label, err)
	}
	select {
	case <-p.Exited():
		return status(p.Err())
	case <-ctx.Done():
	}
	r.signal(ctx, name, pid, "TERM")
	select {
	case <-p.Exited():
	case <-time.After(r.c.Grace):
		r.signal(ctx, name, pid, "KILL")
		p.Stop()
	}
	return fmt.Errorf("%s: %s: %w", name, argv[0], ctx.Err())
}

func (r *Remote) signal(ctx context.Context, name, pid, sig string) {
	cctx, cancel := CleanupCtx(ctx)
	defer cancel()
	// a failure (the process hadn't written its pid yet, or has exited) leaves the kill after
	// Grace, and then Destroy.
	_, _ = r.Output(cctx, name, "sh", "-c", SignalScript, pid, sig)
}

// ExitError maps the exec command's exit to *workspace.ExitError when it's a non-zero
// status, and to an exec failure under label otherwise.
func ExitError(name, label string, err error) error {
	if err == nil {
		return nil
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) && ec.ExitCode() > 0 {
		return fmt.Errorf("%s: %w", name, &workspace.ExitError{Code: ec.ExitCode()})
	}
	return fmt.Errorf("%s: %s: %w", name, label, err)
}
