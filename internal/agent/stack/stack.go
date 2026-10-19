// Package stack brings the local CCF stack up and down for a task
// (design/workstreams/agent/LLD/local-stack.md): local-dev's `make demo-up-<profile>` with
// the component versions pinned through local-dev's overrides, under a compose project of
// its own (factory-<taskID>), one stack per machine.
//
// The one-stack lock is a file (<stateDir>/stack.lock, created exclusively) that holds the
// running stack's State: it outlives the process that brought the stack up, so
// `factory-agent stack wait|env|down` find the stack `stack up` (or the worker) started.
// Every command runs through a Runner, which tests fake: nothing here starts docker by
// itself.
package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
)

// Defaults.
const (
	// DefaultUIURL and DefaultAPIURL are where local-dev publishes the UI and the API.
	DefaultUIURL  = "http://localhost:8000"
	DefaultAPIURL = "http://localhost:8080"
	// DefaultWaitTimeout bounds Wait.
	DefaultWaitTimeout = 10 * time.Minute
	// DefaultPollInterval is Wait's polling interval.
	DefaultPollInterval = 2 * time.Second
	// DefaultStateDir holds the lock.
	DefaultStateDir = "~/.factory-agent"
	// LockName is the lock file in the state directory.
	LockName = "stack.lock"
)

// The profiles: local-dev's demo-up-<profile> targets.
const (
	// ProfileShowcase is the default: realistic seeded data for GIFs.
	ProfileShowcase = "showcase"
	ProfileFree     = "free"
)

// Profiles lists the known profiles.
func Profiles() []string { return []string{ProfileShowcase, ProfileFree} }

// The variables a stack adds to a run's environment (and `stack env` prints).
const (
	EnvURL      = "FACTORY_STACK_URL"
	EnvAPI      = "FACTORY_STACK_API"
	EnvProject  = "FACTORY_STACK_PROJECT"
	EnvVersions = "FACTORY_STACK_VERSIONS"
	// EnvDir is the state directory, so `factory-agent stack` inside a run (whose HOME is a
	// scratch directory) finds the lock.
	EnvDir = "FACTORY_STACK_DIR"
)

// composeCommand is the compose CLI local-dev is told to use (its Makefile defaults to
// podman-compose).
const composeCommand = "docker compose"

// downStopTimeout is compose down's per-container stop timeout in seconds: local-dev's
// workers ask for a 40 s grace period, and the volumes are removed anyway.
const downStopTimeout = "10"

// apiHealthPath is the API's liveness endpoint.
const apiHealthPath = "/api/health"

// projectRE is a compose project name as ProjectName makes it.
var projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// ProjectName is the compose project of a task's stack: factory-<taskID>, lower-cased, with
// every character outside [a-z0-9-] replaced by "-" (compose allows underscores too, but
// the API container's name is also a DNS name, KIND_HOST_ALIAS).
func ProjectName(taskID string) string {
	var b strings.Builder
	b.WriteString("factory-")
	for _, r := range strings.ToLower(taskID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// State is the stack that is up: the lock file's content.
type State struct {
	Project   string        `json:"project"`
	Task      string        `json:"task,omitzero"`
	Profile   string        `json:"profile"`
	LocalDev  string        `json:"localDev"`
	Versions  StackVersions `json:"versions"`
	Source    string        `json:"source,omitzero"`
	StartedAt time.Time     `json:"startedAt,omitzero"`
}

func (s State) validate() error {
	if !projectRE.MatchString(s.Project) {
		return fmt.Errorf("compose project %q: want [a-z0-9-]", s.Project)
	}
	if !slices.Contains(Profiles(), s.Profile) {
		return fmt.Errorf("profile %q: want one of %s", s.Profile, strings.Join(Profiles(), ", "))
	}
	if s.LocalDev == "" || !filepath.IsAbs(s.LocalDev) {
		return fmt.Errorf("localDev %q: want an absolute path", s.LocalDev)
	}
	if s.Versions.API == "" || s.Versions.UI == "" || s.Versions.Agent == "" {
		return errors.New("the api, ui and agent versions must be resolved")
	}
	return s.Versions.Validate()
}

// BusyError is an Up refused because another stack is up on this machine.
type BusyError struct {
	Lock   string
	Holder string // the project up, or "" when the lock can't be read
}

func (e *BusyError) Error() string {
	who := "another stack"
	if e.Holder != "" {
		who = "stack " + e.Holder
	}
	return fmt.Sprintf("%s is up on this machine (%s): one stack per machine; `factory-agent stack down` stops it", who, e.Lock)
}

// IsBusy reports whether err is a BusyError.
func IsBusy(err error) bool {
	var b *BusyError
	return errors.As(err, &b)
}

// ErrNoStack is Current's error when no stack is up.
var ErrNoStack = errors.New("no stack is up")

// Var is one override passed to make.
type Var struct{ Key, Value string }

// Plan is the make invocation that brings a stack up.
type Plan struct {
	Dir    string
	Target string
	Vars   []Var
	// Plugins are the plugin versions asked for, which local-dev doesn't take from its
	// environment.
	Plugins map[string]string
}

// NewPlan is the plan for a state (validated). The overrides go on make's command line,
// where they win over local-dev's .env (the Makefile includes it, and a makefile assignment
// beats the environment), and make exports them to the recipes; they are also set in make's
// environment.
func NewPlan(s State) (Plan, error) {
	if err := s.validate(); err != nil {
		return Plan{}, err
	}
	vars := []Var{
		{"COMPOSE_COMMAND", composeCommand},
		{"COMPOSE_PROJECT_NAME", s.Project},
		{"CCF_API_IMAGE", "ghcr.io/" + Org + "/api:" + s.Versions.API},
		{"CCF_UI_IMAGE", "ghcr.io/" + Org + "/ui:" + s.Versions.UI},
		{"CCF_AGENT_VERSION", s.Versions.Agent},
	}
	if s.Profile == ProfileShowcase {
		// The showcase kind cluster's agent reaches the API by its container name, which is
		// <project>-api-1 (ccf-api-1 under local-dev's own project).
		vars = append(vars, Var{"KIND_HOST_ALIAS", s.Project + "-api-1"})
	}
	return Plan{Dir: s.LocalDev, Target: "demo-up-" + s.Profile, Vars: vars, Plugins: maps.Clone(s.Versions.Plugins)}, nil
}

// env is the plan's overrides as KEY=VALUE.
func (p Plan) env() []string {
	out := make([]string, len(p.Vars))
	for i, v := range p.Vars {
		out[i] = v.Key + "=" + v.Value
	}
	return out
}

// Command is the make command.
func (p Plan) Command() Command {
	return Command{Dir: p.Dir, Name: "make", Args: append([]string{p.Target}, p.env()...), Env: p.env()}
}

// downCommands are the commands that remove a stack: its compose project with its volumes,
// and the showcase's kind cluster.
func downCommands(s State) []Command {
	cmds := []Command{{
		Dir: s.LocalDev, Name: "docker",
		Args: []string{"compose", "--project-name", s.Project, "down", "--volumes", "--remove-orphans", "--timeout", downStopTimeout},
	}}
	if s.Profile == ProfileShowcase {
		cmds = append(cmds, Command{Dir: s.LocalDev, Name: "make", Args: []string{"showcase-kind-destroy", "COMPOSE_COMMAND=" + composeCommand}})
	}
	return cmds
}

// Format writes the plan of s for --dry-run: the project and versions, the directory, the
// environment overrides, the exact make command line, and the down commands.
func (p Plan) Format(w io.Writer, s State) error {
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s\n", s.Project)
	if s.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", s.Source)
	}
	fmt.Fprintf(&b, "versions: api %s, ui %s, agent %s\n", s.Versions.API, s.Versions.UI, s.Versions.Agent)
	fmt.Fprintf(&b, "dir: %s\n", p.Dir)
	b.WriteString("env:\n")
	for _, kv := range p.env() {
		fmt.Fprintf(&b, "  %s\n", kv)
	}
	fmt.Fprintf(&b, "up: %s\n", p.Command().String())
	for _, c := range downCommands(s) {
		fmt.Fprintf(&b, "down: %s\n", c.String())
	}
	if len(p.Plugins) > 0 {
		b.WriteString("plugins (not pinned: local-dev's demo agent configs fix their versions):\n")
		for _, name := range slices.Sorted(maps.Keys(p.Plugins)) {
			fmt.Fprintf(&b, "  %s %s\n", name, p.Plugins[name])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Env is what a stack adds to a run's environment.
func (s State) Env(uiURL, apiURL, stateDir string) map[string]string {
	v, _ := json.Marshal(s.Versions)
	return map[string]string{
		EnvURL: uiURL, EnvAPI: apiURL, EnvProject: s.Project, EnvVersions: string(v), EnvDir: stateDir,
	}
}

// FormatEnv writes env as KEY=VALUE lines, sorted, each value quoted for a POSIX shell when
// it needs it (so `eval "$(factory-agent stack env)"` works).
func FormatEnv(w io.Writer, env map[string]string) error {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "%s=%s\n", k, shellQuote(env[k]))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Command is one program to run.
type Command struct {
	Dir  string
	Name string
	Args []string
	// Env is added to the process's environment (replacing the same keys).
	Env []string
}

// String is the command as a shell line.
func (c Command) String() string {
	parts := []string{shellQuote(c.Name)}
	for _, a := range c.Args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

func shellQuote(s string) string {
	if shellSafeRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Runner runs commands; out gets their stdout and stderr.
type Runner interface {
	Run(ctx context.Context, c Command, out io.Writer) error
}

// ExecRunner runs real programs with the process's environment plus the command's.
type ExecRunner struct {
	// Environ is the base environment; default os.Environ.
	Environ func() []string
}

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, c Command, out io.Writer) error {
	environ := e.Environ
	if environ == nil {
		environ = os.Environ
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = mergeEnv(environ(), c.Env)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", c.Name, firstArg(c.Args), err)
	}
	return nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// mergeEnv is base with over's keys replaced.
func mergeEnv(base, over []string) []string {
	keys := map[string]bool{}
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		keys[k] = true
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, over...)
}

// Options configure a Manager.
type Options struct {
	// StateDir holds the lock; required, absolute.
	StateDir string
	// Runner runs make and docker; default ExecRunner.
	Runner Runner
	// Output gets the commands' output; default discarded.
	Output io.Writer
	// HTTPClient polls the stack in Wait; default a client with a 5 s timeout.
	HTTPClient *http.Client
	// Clock times Wait; default clock.Real.
	Clock clock.Clock
	// UIURL and APIURL are where the stack answers; defaults DefaultUIURL, DefaultAPIURL.
	UIURL, APIURL string
	// PollInterval is Wait's interval; default DefaultPollInterval.
	PollInterval time.Duration
}

// Manager brings stacks up and down on this machine, one at a time.
type Manager struct{ o Options }

// New checks the options and fills in the defaults.
func New(o Options) (*Manager, error) {
	if o.StateDir == "" || !filepath.IsAbs(o.StateDir) {
		return nil, fmt.Errorf("stack: the state directory %q must be absolute", o.StateDir)
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.UIURL == "" {
		o.UIURL = DefaultUIURL
	}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	return &Manager{o: o}, nil
}

// WithOutput is m with the commands' output going to w.
func (m *Manager) WithOutput(w io.Writer) *Manager {
	o := m.o
	o.Output = w
	return &Manager{o: o}
}

// Lock is the lock file's path.
func (m *Manager) Lock() string { return filepath.Join(m.o.StateDir, LockName) }

// StateDir is the state directory.
func (m *Manager) StateDir() string { return m.o.StateDir }

// Env is what the stack s adds to a run's environment.
func (m *Manager) Env(s State) map[string]string { return s.Env(m.o.UIURL, m.o.APIURL, m.o.StateDir) }

// Current is the stack that is up; ErrNoStack when none is.
func (m *Manager) Current() (State, error) {
	b, err := os.ReadFile(m.Lock())
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNoStack
	}
	if err != nil {
		return State{}, fmt.Errorf("the stack lock: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("the stack lock %s: %w", m.Lock(), err)
	}
	return s, nil
}

// Up takes the lock for s and runs its plan. A stack of the same project already up is
// brought up again with s's versions (local-dev's targets are idempotent); a stack of
// another project is a BusyError. When make fails, the stack is taken down and the lock
// released: the caller only calls Down after a successful Up.
func (m *Manager) Up(ctx context.Context, s State) error {
	plan, err := NewPlan(s)
	if err != nil {
		return err
	}
	if err := m.acquire(s); err != nil {
		return err
	}
	if err := m.o.Runner.Run(ctx, plan.Command(), m.o.Output); err != nil {
		if derr := m.Down(context.WithoutCancel(ctx), s.Project); derr != nil {
			err = errors.Join(err, fmt.Errorf("and taking it down: %w", derr))
		}
		return fmt.Errorf("stack up %s: %w", s.Project, err)
	}
	return nil
}

// acquire creates the lock holding s, or replaces it when it already holds s's project.
func (m *Manager) acquire(s State) error {
	if err := os.MkdirAll(m.o.StateDir, 0o700); err != nil {
		return fmt.Errorf("the stack state directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// A lock removed between a failed create and its read is retried.
	for range 3 {
		f, err := os.OpenFile(m.Lock(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(data)
			cerr := f.Close()
			if werr = errors.Join(werr, cerr); werr != nil {
				_ = os.Remove(m.Lock())
				return fmt.Errorf("the stack lock: %w", werr)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("the stack lock: %w", err)
		}
		cur, err := m.Current()
		switch {
		case errors.Is(err, ErrNoStack):
			continue
		case err != nil:
			// Being written by another process, or damaged: either way not ours.
			return &BusyError{Lock: m.Lock()}
		case cur.Project != s.Project:
			return &BusyError{Lock: m.Lock(), Holder: cur.Project}
		}
		return writeFileAtomic(m.Lock(), data)
	}
	return &BusyError{Lock: m.Lock()}
}

func writeFileAtomic(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-")
	if err != nil {
		return fmt.Errorf("the stack lock: %w", err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("the stack lock: %w", err)
	}
	if err := os.Rename(f.Name(), p); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("the stack lock: %w", err)
	}
	return nil
}

// Down removes the stack that is up (compose down --volumes for its project, and the
// showcase's kind cluster) and releases the lock. project, when set, must be the one up.
// With no stack up it does nothing. When a command fails the lock is kept: the stack may
// still be running, and `factory-agent stack down` retries.
func (m *Manager) Down(ctx context.Context, project string) error {
	s, err := m.Current()
	if errors.Is(err, ErrNoStack) {
		return nil
	}
	if err != nil {
		return err
	}
	if project != "" && s.Project != project {
		return fmt.Errorf("the stack up is %s, not %s", s.Project, project)
	}
	if s.LocalDev == "" || !projectRE.MatchString(s.Project) {
		return fmt.Errorf("the stack lock %s names no project or local-dev checkout", m.Lock())
	}
	var errs []error
	for _, c := range downCommands(s) {
		if err := m.o.Runner.Run(ctx, c, m.o.Output); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("stack down %s: %w", s.Project, err)
	}
	if err := os.Remove(m.Lock()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the stack lock: %w", err)
	}
	return nil
}

// Wait polls until the API's health endpoint and the UI both answer 200, for at most
// timeout (DefaultWaitTimeout when ≤ 0).
func (m *Manager) Wait(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	deadline := m.o.Clock.NewTimer(timeout)
	defer deadline.Stop()
	urls := []string{strings.TrimRight(m.o.APIURL, "/") + apiHealthPath, strings.TrimRight(m.o.UIURL, "/") + "/"}
	for {
		problem := ""
		for _, u := range urls {
			if err := m.get(ctx, u); err != nil {
				problem = err.Error()
				break
			}
		}
		if problem == "" {
			return nil
		}
		tick := m.o.Clock.NewTimer(m.o.PollInterval)
		select {
		case <-ctx.Done():
			tick.Stop()
			return fmt.Errorf("waiting for the stack: %w (last: %s)", context.Cause(ctx), problem)
		case <-deadline.C():
			tick.Stop()
			return fmt.Errorf("the stack wasn't ready within %s: %s", timeout, problem)
		case <-tick.C():
		}
	}
}

func (m *Manager) get(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := m.o.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return nil
}

// tail keeps the last n bytes written to it.
type tail struct {
	n   int
	buf bytes.Buffer
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.n; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tail) String() string { return t.buf.String() }
