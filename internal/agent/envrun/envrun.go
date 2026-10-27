// Package envrun brings a task's VM workspace environment up and down (P1 §4,
// workspace-run.md §internal/agent/envrun): it runs spec.environment's setup and up scripts,
// waits for its ready checks, and runs its down script at the end. It implements
// pipeline.Environment; only the VM workspace (vmrun) uses it.
package envrun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/internal/execfile"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// Defaults and bounds.
const (
	// DefaultReadyTimeout bounds the ready checks when ready.timeout is unset.
	DefaultReadyTimeout = 15 * time.Minute
	// KindImagesDir is where the warm base saves warm.kindImages as archives
	// (workspace-host.md §The VM steps); FACTORY_KIND_IMAGES names it when it holds one.
	KindImagesDir = "/var/lib/factory/kind-images"
	// EnvKindImages is the scripts' variable for KindImagesDir.
	EnvKindImages = "FACTORY_KIND_IMAGES"
	// MaxLog caps out/environment.log; past it a [truncated] line ends it.
	MaxLog = 10 << 20
	// MaxTail is how much of a failed script's output its error carries.
	MaxTail = 8 << 10
)

const (
	readyInterval = 2 * time.Second  // between two rounds of ready checks
	readyRequest  = 10 * time.Second // bounds one ready request
	killGrace     = 5 * time.Second  // a cancelled script's group gets SIGKILL this long after SIGTERM
	outputGrace   = 5 * time.Second  // how long a script's output is waited for after it exits
	bash          = "/bin/bash"
)

// Options configure a Runner. StateDir is required.
type Options struct {
	// StateDir holds what the runner writes outside the workspace: the scripts, under env/.
	StateDir string
	// DownTimeout bounds the down script. Default: pipeline.StackDownTimeout.
	DownTimeout time.Duration
	// KindImages is the kind image archives' directory. Default: KindImagesDir.
	KindImages string
	// Clock times the steps and the ready checks. Default: clock.Real.
	Clock clock.Clock
	// Logger logs the steps. Default: discard.
	Logger *slog.Logger
	// OnStep, when set, is called after each sub-step that ran (environment.setup, .up,
	// .ready, .down) with its duration and its redacted error.
	OnStep func(name string, d time.Duration, err error)
	// Redact removes secrets from the scripts' output and the errors. Default:
	// pipeline.RedactShapes (the VM holds no token of its own).
	Redact func(string) string
}

// Runner is one run's environment: Up once, then Down once. It implements
// pipeline.Environment.
type Runner struct {
	o      Options
	client *http.Client
	maxLog int64         // MaxLog; tests lower it
	grace  time.Duration // outputGrace; tests lower it

	mu   sync.Mutex
	root string   // the workspace root: the scripts' working directory
	env  []string // the scripts' environment
	down string   // the down script's file, "" when there is none
	log  *logWriter
	done bool // Down ran
}

var _ pipeline.Environment = (*Runner)(nil)

// New checks the options and fills in the defaults.
func New(o Options) (*Runner, error) {
	if o.StateDir == "" || !filepath.IsAbs(o.StateDir) {
		return nil, fmt.Errorf("envrun: StateDir %q must be absolute", o.StateDir)
	}
	if o.DownTimeout <= 0 {
		o.DownTimeout = pipeline.StackDownTimeout
	}
	if o.KindImages == "" {
		o.KindImages = KindImagesDir
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Redact == nil {
		o.Redact = pipeline.RedactShapes
	}
	// The ready URLs are the VM's own services (localhost): never through the gateway's proxy.
	// A redirect isn't followed: a 3xx is ready.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Runner{o: o, client: client, maxLog: MaxLog, grace: outputGrace}, nil
}

// Up implements pipeline.Environment: it writes the scripts, runs setup and up in ws.Root, and
// waits for ready. It returns the exposes and FACTORY_VERSION_<COMPONENT> for each
// spec.versions entry.
func (r *Runner) Up(ctx context.Context, ws harness.Workspace, t v1alpha1.Task) (map[string]string, error) {
	// The dispatcher validated the Task, but the wire isn't trusted: exposes and the version
	// names become variables.
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("the task: %w", err)
	}
	e := t.Spec.Environment
	if e == nil {
		e = &v1alpha1.Environment{}
	}
	vars := maps.Clone(e.Exposes)
	if vars == nil {
		vars = map[string]string{}
	}
	for c, v := range t.Spec.Versions {
		vars[VersionVar(c)] = v
	}

	env := slices.Clone(ws.Env)
	env = append(env, pipeline.EnvTaskID+"="+t.Metadata.ID)
	for _, k := range slices.Sorted(maps.Keys(t.Spec.Versions)) {
		env = append(env, VersionVar(k)+"="+t.Spec.Versions[k])
	}
	if hasArchive(r.o.KindImages) {
		env = append(env, EnvKindImages+"="+r.o.KindImages)
	}

	lw, err := openLog(filepath.Join(ws.Out, runbundle.EnvironmentLog), r.maxLog, r.o.Redact)
	if err != nil {
		return nil, err
	}
	scripts, err := writeScripts(filepath.Join(r.o.StateDir, "env"), e)
	if err != nil {
		_ = lw.Close()
		return nil, err
	}
	r.mu.Lock()
	r.root, r.env, r.down, r.log = ws.Root, env, scripts["down"], lw
	r.mu.Unlock()

	for _, name := range []string{"setup", "up"} {
		if p := scripts[name]; p != "" {
			if err := r.step(name, func() error { return r.script(ctx, name, p) }); err != nil {
				return nil, err
			}
		}
	}
	if e.Ready != nil && len(e.Ready.HTTP) > 0 {
		timeout := DefaultReadyTimeout
		if e.Ready.Timeout != "" {
			if d, err := e.Ready.Timeout.Parse(); err == nil && d > 0 {
				timeout = d
			}
		}
		if err := r.step("ready", func() error { return r.ready(ctx, e.Ready.HTTP, timeout) }); err != nil {
			return nil, err
		}
	}
	return vars, nil
}

// Down implements pipeline.Environment: it runs the down script once, bounded by DownTimeout
// and ctx, whichever ends first, and closes the log. Its error is the environment.down step's,
// never the run's outcome.
func (r *Runner) Down(ctx context.Context) error {
	r.mu.Lock()
	if r.done || r.log == nil {
		r.mu.Unlock()
		return nil
	}
	r.done = true
	down, lw := r.down, r.log
	r.mu.Unlock()
	defer func() { _ = lw.Close() }()
	if down == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.o.DownTimeout)
	defer cancel()
	return r.step("down", func() error { return r.script(ctx, "down", down) })
}

// step runs fn as environment.<name>, reporting it to OnStep.
func (r *Runner) step(name string, fn func() error) error {
	t0 := r.o.Clock.Now()
	r.o.Logger.Info("environment step starting", "step", name)
	err := fn()
	if err != nil {
		err = &redactedError{msg: r.o.Redact(err.Error()), err: err}
		r.o.Logger.Warn("environment step failed", "step", name, "err", err.Error())
	}
	if f := r.o.OnStep; f != nil {
		f("environment."+name, r.o.Clock.Now().Sub(t0), err)
	}
	return err
}

// script runs one script as `bash --noprofile --norc -eo pipefail <file>` in its own process
// group, its output in the log. Cancelling ctx sends the group SIGTERM, then SIGKILL after
// killGrace. A command the script leaves running (a server started with &) keeps running and
// keeps writing to the log: the output is a pipe the runner reads until its last writer
// closes it, waiting for it at most outputGrace after the script exits.
func (r *Runner) script(ctx context.Context, name, path string) error {
	r.mu.Lock()
	root, env, lw := r.root, r.env, r.log
	r.mu.Unlock()
	lw.begin(name)
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%s: the output pipe: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-eo", "pipefail", path)
	cmd.Dir, cmd.Env = root, env
	cmd.Stdout, cmd.Stderr = pw, pw // a file, so Wait doesn't wait for (or close) the copy
	inOwnGroup(cmd)
	cmd.Cancel = func() error { return terminateGroup(cmd.Process.Pid) }
	cmd.WaitDelay = killGrace
	err = cmd.Start()
	_ = pw.Close() // the script and what it starts hold the write end
	if err != nil {
		_ = pr.Close()
		return fmt.Errorf("%s: start: %w", name, err)
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(lw, pr)
		_ = pr.Close()
		close(copied)
	}()
	err = cmd.Wait()
	if ctx.Err() != nil {
		_ = killGroup(cmd.Process.Pid)
	}
	grace := time.NewTimer(r.grace) // an I/O bound, not a decision: the real clock
	select {
	case <-copied:
	case <-grace.C:
	}
	grace.Stop()
	tail := lw.end()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (%w)", err, context.Cause(ctx))
	}
	if tail == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w; its output ends:\n%s", name, err, tail)
}

// ready polls each URL every readyInterval until it answers 2xx or 3xx; all of them must
// within timeout, on the injected clock.
func (r *Runner) ready(ctx context.Context, urls []string, timeout time.Duration) error {
	deadline := r.o.Clock.NewTimer(timeout)
	defer deadline.Stop()
	defer r.client.CloseIdleConnections()
	pending := slices.Clone(urls)
	last := map[string]string{}
	for {
		pending = slices.DeleteFunc(pending, func(u string) bool {
			ok, why := r.check(ctx, u)
			last[u] = why
			return ok
		})
		if len(pending) == 0 {
			return nil
		}
		tick := r.o.Clock.NewTimer(readyInterval)
		select {
		case <-ctx.Done():
			tick.Stop()
			return fmt.Errorf("ready: %w", context.Cause(ctx))
		case <-deadline.C():
			tick.Stop()
			return fmt.Errorf("ready: %s isn't ready after %s (last: %s)", pending[0], timeout, last[pending[0]])
		case <-tick.C():
		}
	}
}

// check is one ready request: ok on 2xx or 3xx, else why not.
func (r *Runner) check(ctx context.Context, u string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, readyRequest)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400, resp.Status
}

// VersionVar is the scripts' and the harness's variable for a spec.versions component:
// FACTORY_VERSION_ and the component upper-cased, with "-" as "_".
func VersionVar(component string) string {
	return v1alpha1.EnvVersionPrefix + strings.ReplaceAll(strings.ToUpper(component), "-", "_")
}

// writeScripts writes each non-empty script to dir/<name>.sh (0700) and returns their files.
func writeScripts(dir string, e *v1alpha1.Environment) (map[string]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("the scripts' directory: %w", err)
	}
	out := map[string]string{}
	for name, body := range map[string]string{"setup": e.Setup, "up": e.Up, "down": e.Down} {
		p := filepath.Join(dir, name+".sh")
		if body == "" {
			continue
		}
		if err := execfile.WriteFile(p, []byte(body), 0o700); err != nil {
			return nil, fmt.Errorf("the %s script: %w", name, err)
		}
		out[name] = p
	}
	return out, nil
}

// hasArchive reports whether dir holds a regular .tar file.
func hasArchive(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".tar")
	})
}

// redactedError is an error whose text was redacted; it unwraps to the original.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
