// Package pipeline is the run pipeline (design/workstreams/agent/LLD/run-pipeline.md): it
// turns one leased Task into a harness run and the run into a validated Result. It
// implements runner.Runner, so the worker's lease loop drives it.
//
// For each job it creates a scratch workspace (0700, always removed), starts an in-memory git
// credential helper (package gitcred) for the task's repos, clones them, materialises the
// pinned skills, renders the prompt (the headless preamble, the rendered template from the
// API, and the context items fenced with their trust level), builds the harness's
// environment from an allow-list, runs the harness through its harness.Adapter under the
// task's limits, validates the Result against the output schema and outcomes (with one
// repair turn), uploads the redacted, gzipped transcript, and cleans up.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Defaults.
const (
	// DefaultWallClock bounds a run whose TaskSpec sets no limits.maxWallClock (the
	// worker's drain deadline uses the same default).
	DefaultWallClock = time.Hour
	// DefaultTranscriptLimit is the API's transcript cap (contracts.md: ≤ 20 MB gzipped).
	DefaultTranscriptLimit = 20 << 20
	// DefaultSkillsBase prefixes a skills source that isn't a URL ("ccf-skills").
	DefaultSkillsBase = "https://github.com/compliance-framework"
	// DefaultBotName and DefaultBotEmail are the commit author in the rw checkout.
	DefaultBotName  = "ccf-factory[bot]"
	DefaultBotEmail = "ccf-factory[bot]@users.noreply.github.com"
)

// cleanupTimeout bounds the work done after the run ended (transcript upload, stack down).
const cleanupTimeout = 30 * time.Second

// SchemaSource gives a task's spec-specific output schema (the TaskSpec's output.schema, a
// JSON Schema document). A nil document means none is known: the Result is then checked
// against the v1alpha1 Result schema and the task's outcomes only.
type SchemaSource interface {
	OutputSchema(ctx context.Context, t v1alpha1.Task) ([]byte, error)
}

// Stack brings the local CCF stack up for a task that needs one (local-stack.md,
// AGT-S1-T06). Up returns the variables to add to the harness's environment
// (FACTORY_STACK_URL, FACTORY_STACK_API); Down is called once in cleanup after a successful
// Up.
type Stack interface {
	Up(ctx context.Context, ws harness.Workspace, t v1alpha1.Task) (map[string]string, error)
	Down(ctx context.Context) error
}

// Options configure a Pipeline. WorkRoot and Adapters are required.
type Options struct {
	// WorkRoot holds the per-task workspaces (workRoot/<taskID>/).
	WorkRoot string
	// Adapters are the harness adapters by harness name (task.harness.resolved.name).
	Adapters map[string]harness.Adapter
	// AgentBinary is the factory-agent executable: the credential helper and the `factory`
	// CLI on the harness's PATH run it. Default: os.Executable().
	AgentBinary string
	// HelperCommand is the credential helper's command line, without the operation git
	// appends. Default: AgentBinary fx git-credential.
	HelperCommand []string
	// Git is the git binary. Default: "git".
	Git string
	// GitBaseURL is where the task's repos are cloned from (<base>/<owner>/<name>.git).
	// Default: https://<the credential's host>.
	GitBaseURL string
	// SkillsBase prefixes a skills source that isn't a URL. Default: DefaultSkillsBase.
	SkillsBase string
	// SkillsCache caches skills checkouts by commit (<cache>/<commit>). Empty: no cache.
	SkillsCache string
	// SocketDir is where the credential socket's directory is created. Default:
	// os.TempDir() (Unix socket paths must stay short).
	SocketDir string
	// Schemas gives spec-specific output schemas; nil: none.
	Schemas SchemaSource
	// Stack brings the local stack up; nil: tasks that need one fail with stack-error.
	Stack Stack
	// Environ is the owner's environment, reduced to the allow-list for the harness.
	// Default: os.Environ.
	Environ func() []string
	// Clock times the run (wall-clock limit, usage); default clock.Real.
	Clock clock.Clock
	// Logger logs the pipeline's steps; never a token. Default: discard.
	Logger *slog.Logger
	// KeepWorkspace keeps the workspace after the run (debugging): it is logged, not
	// removed. The credential socket is removed anyway.
	KeepWorkspace bool
	// TranscriptLimit caps the gzipped transcript. Default: DefaultTranscriptLimit.
	TranscriptLimit int64
	// DefaultWallClock applies when the task has no maxWallClock. Default:
	// DefaultWallClock.
	DefaultWallClock time.Duration
	// BotName and BotEmail are the commit author set in the writable checkout.
	BotName, BotEmail string
}

// Pipeline runs leased tasks. It is safe for concurrent use: each run has its own
// workspace.
type Pipeline struct {
	o   Options
	log *slog.Logger
	clk clock.Clock

	skillsMu sync.Mutex
	skillsLk map[string]*sync.Mutex
}

var _ runner.Runner = (*Pipeline)(nil)

// New checks the options and fills in the defaults.
func New(o Options) (*Pipeline, error) {
	if o.WorkRoot == "" {
		return nil, errors.New("pipeline: WorkRoot is required")
	}
	if !filepath.IsAbs(o.WorkRoot) {
		return nil, fmt.Errorf("pipeline: WorkRoot %q must be absolute", o.WorkRoot)
	}
	if len(o.Adapters) == 0 {
		return nil, errors.New("pipeline: no harness adapters")
	}
	if o.AgentBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("pipeline: the agent binary: %w", err)
		}
		o.AgentBinary = exe
	}
	if len(o.HelperCommand) == 0 {
		o.HelperCommand = []string{o.AgentBinary, "fx", "git-credential"}
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.SkillsBase == "" {
		o.SkillsBase = DefaultSkillsBase
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.TranscriptLimit <= 0 {
		o.TranscriptLimit = DefaultTranscriptLimit
	}
	if o.DefaultWallClock <= 0 {
		o.DefaultWallClock = DefaultWallClock
	}
	if o.BotName == "" {
		o.BotName = DefaultBotName
	}
	if o.BotEmail == "" {
		o.BotEmail = DefaultBotEmail
	}
	return &Pipeline{o: o, log: o.Logger, clk: o.Clock, skillsLk: map[string]*sync.Mutex{}}, nil
}

// taskIDRE is a task id safe as a directory name.
var taskIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Why a run's context ended (context.Cause), besides the worker's own causes.
var (
	errThrottled = errors.New("the harness is rate limited")
)

// Run implements runner.Runner (run-pipeline.md §Steps).
func (p *Pipeline) Run(ctx context.Context, job *runner.Job, sink runner.Sink) (runner.Outcome, error) {
	task := job.Lease.Task
	id := task.Metadata.ID
	if !taskIDRE.MatchString(id) || strings.Contains(id, "..") {
		return runner.Outcome{}, fmt.Errorf("task id %q can't name a workspace", id)
	}
	start := job.StartedAt
	if start.IsZero() {
		start = p.clk.Now()
	}
	r := &run{p: p, job: job, task: task, id: id, start: start, log: p.log.With("task", id)}
	r.red = newRedactor(job)
	r.mon = &monitor{sink: sink, clk: p.clk, red: r.red}

	wall := p.o.DefaultWallClock
	if l := task.Spec.Limits; l != nil && l.MaxWallClock != "" {
		d, err := l.MaxWallClock.Parse()
		if err != nil || d <= 0 {
			return runner.Outcome{}, fmt.Errorf("limits.maxWallClock %q: invalid", l.MaxWallClock)
		}
		wall = d
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.mon.cancel = cancel
	if l := task.Spec.Limits; l != nil {
		r.mon.maxTurns, r.mon.maxCost = l.MaxTurns, l.MaxCostUSD
	}
	timer := p.clk.NewTimerAt(start.Add(wall))
	defer timer.Stop()
	go func() {
		select {
		case <-timer.C():
			r.mon.stop(&harness.LimitError{Limit: harness.LimitWallClock, Detail: "the run took longer than " + wall.String()})
		case <-runCtx.Done():
		}
	}()

	defer r.cleanup(ctx)
	out := r.execute(runCtx)
	return r.finish(ctx, out), nil
}

// run is one job's state.
type run struct {
	p     *Pipeline
	job   *runner.Job
	task  v1alpha1.Task
	id    string
	start time.Time
	log   *slog.Logger
	red   *redactor
	mon   *monitor

	ws        harness.Workspace
	creds     *gitcred.Server
	base      *url.URL // where repos are cloned from
	protocols string   // GIT_ALLOW_PROTOCOL
	stackUp   bool
	stackEnv  map[string]string
	adapter   harness.Adapter
	harnessed bool // the harness was started
	prompt    string
	check     *resultCheck
	git       *gitRunner
	parts     [][]byte // transcript parts, in order
	result    *v1alpha1.Result
}

// step outcomes: execute returns the outcome of the step that ended the run.
type stepResult struct {
	status, reason string
	err            error
}

func failed(reason string, err error) stepResult {
	return stepResult{status: runner.StatusFailed, reason: reason, err: err}
}

// execute runs the steps up to the validated Result.
func (r *run) execute(ctx context.Context) stepResult {
	h := r.task.Spec.Harness
	if h == nil || h.Resolved == nil || h.Resolved.Name == "" {
		return failed(runner.ReasonHarnessError, errors.New("the task has no resolved harness"))
	}
	ad, ok := r.p.o.Adapters[h.Resolved.Name]
	if !ok {
		return failed(runner.ReasonHarnessError, fmt.Errorf("no adapter for harness %q", h.Resolved.Name))
	}
	r.adapter = ad

	steps := []struct {
		name   string
		reason string
		fn     func(context.Context) error
	}{
		{"workspace", runner.ReasonHarnessError, r.makeWorkspace},
		{"credentials", runner.ReasonHarnessError, r.startCredentials},
		{"clone", runner.ReasonHarnessError, r.cloneRepos},
		{"stack", runner.ReasonStackError, r.stackUpIfNeeded},
		{"skills", runner.ReasonHarnessError, r.materialiseSkills},
		{"schema", runner.ReasonHarnessError, r.prepareSchema},
		{"prompt", runner.ReasonHarnessError, r.renderPrompt},
		{"environment", runner.ReasonHarnessError, r.buildEnv},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return failed(s.reason, fmt.Errorf("%s: %w", s.name, err))
		}
		r.log.Debug("pipeline step done", "step", s.name)
	}
	if err := ad.Prepare(ctx, r.ws, r.task); err != nil {
		return failed(runner.ReasonHarnessError, fmt.Errorf("prepare %s: %w", ad.Name(), err))
	}
	return r.runHarness(ctx)
}

// runHarness runs the harness, validates the Result and, once, repairs it.
func (r *run) runHarness(ctx context.Context) stepResult {
	r.harnessed = true
	r.mon.countTurns(true)
	r.log.Info("harness starting", "harness", r.adapter.Name())
	raw, err := r.adapter.Run(ctx, r.ws, r.task, r.prompt, r.mon)
	r.parts = append(r.parts, raw.Transcript)
	r.mon.countTurns(false)
	if res, stop := r.harnessEnded(ctx, err); stop {
		return res
	}
	res, problems := r.checkResult(raw)
	if len(problems) == 0 {
		r.result = res
		return stepResult{status: string(res.Status)}
	}
	r.log.Info("the result is invalid: one repair turn", "problems", len(problems))
	r.parts = append(r.parts, factoryLine("repair", map[string]any{"problems": problems}))
	if err := os.Remove(r.ws.Result); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed(runner.ReasonHarnessError, fmt.Errorf("remove the invalid result: %w", err))
	}
	raw, err = r.adapter.Repair(ctx, r.ws, problems, r.mon)
	r.parts = append(r.parts, raw.Transcript)
	if res, stop := r.harnessEnded(ctx, err); stop {
		return res
	}
	res, problems = r.checkResult(raw)
	if len(problems) > 0 {
		r.parts = append(r.parts, factoryLine("invalid-result", map[string]any{"problems": problems}))
		return failed(runner.ReasonInvalidResult, fmt.Errorf("still invalid after the repair turn: %s", strings.Join(problems, "; ")))
	}
	r.result = res
	return stepResult{status: string(res.Status)}
}

// harnessEnded classifies how a Run or Repair returned: stop is true when the run can't go
// on to the Result (a limit, a rate limit, a cancel, a harness error).
func (r *run) harnessEnded(ctx context.Context, err error) (stepResult, bool) {
	if cause := r.mon.stopped(); cause != nil {
		return stepFromCause(cause), true
	}
	if ctx.Err() != nil {
		return stepFromCause(context.Cause(ctx)), true
	}
	if err != nil {
		if harness.IsLimit(err) {
			return failed(runner.ReasonLimit, err), true
		}
		return failed(runner.ReasonHarnessError, fmt.Errorf("%s: %w", r.adapter.Name(), err)), true
	}
	return stepResult{}, false
}

func stepFromCause(cause error) stepResult {
	switch {
	case errors.Is(cause, errThrottled):
		return stepResult{status: runner.StatusThrottled, err: cause}
	case harness.IsLimit(cause):
		return failed(runner.ReasonLimit, cause)
	}
	return failed(runner.ReasonCancelled, cause)
}

// finish turns the last step into the Outcome, uploading the transcript when the harness
// ran. parent is the worker's context.
func (r *run) finish(parent context.Context, s stepResult) runner.Outcome {
	// A step that failed because a limit or a rate limit cancelled it (the clone was
	// cancelled by the wall-clock timer) ended for that reason.
	if cause := r.mon.stopped(); cause != nil && s.status == runner.StatusFailed &&
		(s.reason == runner.ReasonHarnessError || s.reason == runner.ReasonStackError || s.reason == runner.ReasonCancelled) {
		s = stepFromCause(cause)
	}
	if parent.Err() != nil && r.result == nil {
		s = failed(runner.ReasonCancelled, context.Cause(parent))
	}
	out := runner.Outcome{Status: s.status, Reason: s.reason}
	if s.status == runner.StatusThrottled {
		if until := r.mon.throttledUntil(); !until.IsZero() {
			out.ThrottledUntil = &until
		}
	}
	usage := r.mon.totals(r.p.clk.Now().Sub(r.start))
	switch {
	case r.result != nil:
		res := *r.result
		res.Harness = r.harnessInfo(usage)
		out.Result = &res
	case s.status == runner.StatusFailed && s.reason != runner.ReasonCancelled:
		summary := s.reason
		if s.err != nil {
			summary += ": " + r.red.String(s.err.Error())
		}
		res := v1alpha1.Result{Task: r.id, Status: v1alpha1.ResultFailed, Summary: truncate(summary, maxSummary)}
		if r.harnessed {
			res.Harness = r.harnessInfo(usage)
		}
		out.Result = &res
	}
	if s.err != nil {
		r.log.Warn("run ended", "status", s.status, "reason", s.reason, "err", r.red.String(s.err.Error()))
	} else {
		r.log.Info("run ended", "status", s.status)
	}
	if r.harnessed && parent.Err() == nil {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
		ref, err := r.uploadTranscript(uctx)
		cancel()
		if err != nil {
			r.log.Warn("transcript not uploaded", "err", r.red.String(err.Error()))
		} else {
			out.TranscriptRef = ref
		}
	}
	return out
}

// harnessInfo is the Result's harness block: the resolved config and the run's usage.
func (r *run) harnessInfo(u v1alpha1.Usage) *v1alpha1.ResultHarness {
	h := r.task.Spec.Harness.Resolved
	return &v1alpha1.ResultHarness{
		Name: h.Name, Version: h.Version, Auth: h.Auth,
		EffectiveConfig: v1alpha1.EffectiveConfig{Model: h.Model, ReasoningEffort: h.ReasoningEffort},
		Usage:           u,
	}
}

// cleanup always runs: stack down, the credential socket closed, the workspace removed
// (run-pipeline.md §Steps 12).
func (r *run) cleanup(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	if r.stackUp {
		if err := r.p.o.Stack.Down(ctx); err != nil {
			r.log.Warn("stack down failed", "err", r.red.String(err.Error()))
		}
	}
	if r.creds != nil {
		if err := r.creds.Close(); err != nil {
			r.log.Warn("closing the credential socket failed", "err", err)
		}
		r.creds = nil
	}
	if r.ws.Root == "" {
		return
	}
	if r.p.o.KeepWorkspace {
		r.log.Warn("keeping the workspace (--keep-workspace)", "dir", r.ws.Root)
		return
	}
	if err := removeAll(r.ws.Root); err != nil {
		r.log.Error("removing the workspace failed", "dir", r.ws.Root, "err", err)
	}
}

// maxSummary bounds a failure summary (the worker's bound).
const maxSummary = 500

// truncate cuts s to n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// monitor is the harness's EventSink: it forwards progress and usage to the worker's sink,
// counts turns and cost against the limits, and stops the run on a limit or a rate limit.
type monitor struct {
	sink   runner.Sink
	clk    clock.Clock
	red    *redactor
	cancel context.CancelCauseFunc

	maxTurns int
	maxCost  float64

	mu       sync.Mutex
	counting bool
	turns    int
	in, out  int64
	cost     *float64
	cause    error
	until    time.Time
}

var _ harness.EventSink = (*monitor)(nil)

func (m *monitor) countTurns(on bool) {
	m.mu.Lock()
	m.counting = on
	m.mu.Unlock()
}

// Progress implements harness.EventSink.
func (m *monitor) Progress(e harness.ProgressEvent) {
	at := e.At
	if at.IsZero() {
		at = m.clk.Now()
	}
	kind := e.Kind
	if kind == "" {
		kind = harness.KindMessage
	}
	m.sink.Progress(v1alpha1.ProgressEvent{At: at.UTC(), Kind: kind, Message: truncate(m.red.String(e.Summary), harness.MaxSummary)})
	if kind != harness.KindTurn {
		return
	}
	m.mu.Lock()
	m.turns++
	over := m.counting && m.maxTurns > 0 && m.turns > m.maxTurns
	m.mu.Unlock()
	if over {
		m.stop(&harness.LimitError{Limit: harness.LimitTurns, Detail: fmt.Sprintf("more than %d turns", m.maxTurns)})
	}
}

// Usage implements harness.EventSink.
func (m *monitor) Usage(u harness.UsageDelta) {
	m.sink.Usage(runner.UsageDelta{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: u.CostUSD})
	m.mu.Lock()
	m.in += u.InputTokens
	m.out += u.OutputTokens
	if u.CostUSD != nil {
		c := *u.CostUSD
		if m.cost != nil {
			c += *m.cost
		}
		m.cost = &c
	}
	over := m.maxCost > 0 && m.cost != nil && *m.cost > m.maxCost
	m.mu.Unlock()
	if over {
		m.stop(&harness.LimitError{Limit: harness.LimitCost, Detail: fmt.Sprintf("more than $%.2f", m.maxCost)})
	}
}

// RateLimited implements harness.EventSink.
func (m *monitor) RateLimited(until time.Time) {
	m.mu.Lock()
	if m.cause == nil {
		m.until = until
	}
	m.mu.Unlock()
	m.stop(errThrottled)
}

// stop records the first reason to stop and cancels the run with it.
func (m *monitor) stop(cause error) {
	m.mu.Lock()
	if m.cause != nil {
		m.mu.Unlock()
		return
	}
	m.cause = cause
	m.mu.Unlock()
	m.cancel(cause)
}

func (m *monitor) stopped() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cause
}

func (m *monitor) throttledUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.until
}

// totals is the run's usage so far.
func (m *monitor) totals(wall time.Duration) v1alpha1.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := v1alpha1.Usage{InputTokens: m.in, OutputTokens: m.out, Turns: m.turns}
	if m.cost != nil {
		c := *m.cost
		u.CostUSD = &c
	}
	if wall > 0 {
		u.WallClock = v1alpha1.Duration(wall.Round(time.Second).String())
	}
	return u
}

// containsLabel reports whether the task requires label.
func containsLabel(t v1alpha1.Task, label string) bool {
	return t.Spec.Requires != nil && slices.Contains(t.Spec.Requires.Labels, label)
}
