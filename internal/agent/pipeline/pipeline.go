// Package pipeline is the run pipeline (design/workstreams/agent/LLD/run-pipeline.md): it
// turns one leased Task into a harness run and the run into a validated Result. It
// implements runner.Runner, so the worker's lease loop drives it.
//
// For each job it creates a scratch workspace (0700, always removed; or the exact Root, kept),
// starts an in-memory git credential helper (package gitcred) for the task's repos (or, in
// gateway mode, none: git goes through the gateway's proxy), clones them, materialises the
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
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// cleanupTimeout bounds the transcript upload after the run ended.
const cleanupTimeout = 30 * time.Second

// StackDownTimeout bounds a VM environment's down script (envrun's DownTimeout, vmrun's
// --down-timeout): local-dev's compose down stops a dozen containers and removes their
// volumes, and the showcase profile also deletes a kind cluster, which takes longer than
// cleanupTimeout. The name predates the environment: it was the host stack's budget.
const StackDownTimeout = 3 * time.Minute

// GitAuth is how the run's git authenticates to the git host (workspace-run.md §Gateway mode).
type GitAuth int

const (
	// GitAuthHelper, the zero value (the host), answers git from the job's credentials through
	// the in-memory credential helper (package gitcred) on a Unix socket.
	GitAuthHelper GitAuth = iota
	// GitAuthGateway (the VM workspace) has no credential helper: no socket and no helper
	// config. git reaches the git host through the gateway (HTTPS_PROXY in Options.Env), which
	// adds the credential for the repo in the path. The job carries no credential: Run refuses
	// one that does (GatewayCredentialError) before any step.
	GitAuthGateway
)

// String implements fmt.Stringer.
func (a GitAuth) String() string {
	switch a {
	case GitAuthHelper:
		return "helper"
	case GitAuthGateway:
		return "gateway"
	}
	return "GitAuth(" + strconv.Itoa(int(a)) + ")"
}

// Options configure a Pipeline. Adapters and exactly one of WorkRoot and Root are required.
type Options struct {
	// WorkRoot holds the per-task workspaces (workRoot/<taskID>/), each removed after its
	// run (the host).
	WorkRoot string
	// Root is the exact workspace root, instead of WorkRoot (the VM workspace: /work). It must
	// be absolute, and empty or absent when a run starts (its parent must exist); the run lays
	// its tree directly in it and cleanup never removes it, so out/ survives the run. One run
	// at a time: a second finds it non-empty and fails its workspace step.
	Root string
	// GitAuth is how the run's git authenticates. Default: GitAuthHelper.
	GitAuth GitAuth
	// Env is added to the environment that the pipeline's own git and the harness share,
	// after the owner's allow-list (so it wins over Environ) and before the pipeline's own
	// variables: the gateway's proxies (HTTP_PROXY, HTTPS_PROXY, NO_PROXY, either case) and CA
	// (SSL_CERT_FILE, GIT_SSL_CAINFO, NODE_EXTRA_CA_CERTS, …), a GH_TOKEN placeholder,
	// DOCKER_CONFIG. New refuses a variable the pipeline sets itself (HOME, TMPDIR, the XDG
	// and PATH ones, the GIT_* and GCM_* ones of the git setup, the credential socket and the
	// FACTORY_* ones of the harness's environment), and one that
	// could add a credential helper or change what git or the shell runs: every GIT_* name
	// but GIT_SSL_CAINFO and GIT_SSL_CAPATH, SSH_ASKPASS, LD_*, DYLD_*, BASH_ENV and ENV. On
	// the host it is usually empty.
	Env map[string]string
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
	// Schemas gives spec-specific output schemas. Default: APISchemas, the dispatcher's
	// getTaskOutputSchema through the job's client.
	Schemas SchemaSource
	// Prompts gives the rendered prompt template and the context items. Default:
	// APIPrompts, the dispatcher's getTaskPrompt and getTaskContext through the job's client.
	Prompts PromptSource
	// Skills gives the run its materialised skills. Default: FetchSkills, the pinned skills
	// repos fetched with the run's git (through SkillsCache) into the workspace's skills/.
	Skills SkillSource
	// Transcripts keeps the redacted, gzipped transcript. Default: APITranscripts, the
	// dispatcher's uploadTranscript through the job's client.
	Transcripts TranscriptStore
	// Environment brings up a task's spec.environment (the VM workspace's envrun); nil on the
	// host, where such a task's environment is skipped with a warning.
	Environment Environment
	// Environ is the owner's environment, reduced to the allow-list for the harness.
	// Default: os.Environ.
	Environ func() []string
	// Clock times the run (wall-clock limit, usage); default clock.Real.
	Clock clock.Clock
	// Logger logs the pipeline's steps; never a token. Default: discard.
	Logger *slog.Logger
	// KeepWorkspace keeps the workspace after the run (debugging): it is logged, not
	// removed. The credential socket is removed anyway. A Root is always kept.
	KeepWorkspace bool
	// TranscriptLimit caps the gzipped transcript. Default: DefaultTranscriptLimit.
	TranscriptLimit int64
	// DefaultWallClock applies when the task has no maxWallClock. Default:
	// DefaultWallClock.
	DefaultWallClock time.Duration
	// BotName and BotEmail are the commit author set in the writable checkout.
	BotName, BotEmail string
	// OnStep, when set, is called after each step that ran with its name, its duration and
	// its error (nil on success): workspace, credentials, clone, environment (only for
	// a task that declares one), skills, schema, prompt, env, prepare, harness, repair (only
	// after an invalid Result) and finish (its error is the transcript's). The error is redacted with the run's redactor
	// (its tokens and anything shaped like one): its Error() is the redacted text, and it
	// unwraps to the step's own error, so errors.Is and errors.As still match. An error
	// unwrapped from it is the original, unredacted: report the error OnStep gets, not one
	// errors.Unwrap or errors.As returns.
	OnStep func(name string, d time.Duration, err error)
	// Finished, when set, is called once with the run's Outcome at the end of finish, after
	// the transcript is stored and before cleanup (the environment's Down, the credential socket,
	// the workspace's removal). It isn't called when Run returns an error.
	Finished func(runner.Outcome)
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
	switch {
	case o.WorkRoot == "" && o.Root == "":
		return nil, errors.New("pipeline: WorkRoot or Root is required")
	case o.WorkRoot != "" && o.Root != "":
		return nil, errors.New("pipeline: WorkRoot and Root are exclusive")
	case o.Root != "" && !filepath.IsAbs(o.Root):
		return nil, fmt.Errorf("pipeline: Root %q must be absolute", o.Root)
	case o.WorkRoot != "" && !filepath.IsAbs(o.WorkRoot):
		return nil, fmt.Errorf("pipeline: WorkRoot %q must be absolute", o.WorkRoot)
	}
	if o.GitAuth != GitAuthHelper && o.GitAuth != GitAuthGateway {
		return nil, fmt.Errorf("pipeline: unknown GitAuth %v", o.GitAuth)
	}
	if err := checkEnv(o.Env); err != nil {
		return nil, fmt.Errorf("pipeline: Env: %w", err)
	}
	o.Env = maps.Clone(o.Env)
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
	if o.Schemas == nil {
		o.Schemas = APISchemas{}
	}
	if o.Prompts == nil {
		o.Prompts = APIPrompts{}
	}
	if o.Skills == nil {
		o.Skills = FetchSkills{}
	}
	if o.Transcripts == nil {
		o.Transcripts = APITranscripts{}
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
	base      *url.URL          // where repos are cloned from
	protocols string            // GIT_ALLOW_PROTOCOL
	envUp     bool              // Environment.Up was called
	envVars   map[string]string // what it returned
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

// GatewayCredentialError is a gateway-mode run's refusal of a job that carries a credential:
// in a VM workspace every credential lives in the gateway, never in the job. Field names the
// field (Lease.GitCredential.Token, Git.Reads[0].Token, AgentToken, …), never its value.
type GatewayCredentialError struct{ Field string }

func (e *GatewayCredentialError) Error() string {
	return "gateway mode: the job carries a credential in " + e.Field
}

// jobCredential is the first credential field the job sets, or "" when it carries none: the
// lease's git and read tokens, the current ones in Git, the agent token and the dispatcher
// client (authenticated as the agent).
func jobCredential(job *runner.Job) string {
	if job.Lease.GitCredential.Token != "" {
		return "Lease.GitCredential.Token"
	}
	for i, c := range job.Lease.ReadCredentials {
		if c.Token != "" {
			return fmt.Sprintf("Lease.ReadCredentials[%d].Token", i)
		}
	}
	if job.Git != nil {
		if job.Git.Get().Token != "" {
			return "Git.Token"
		}
		for i, c := range job.Git.Reads() {
			if c.Token != "" {
				return fmt.Sprintf("Git.Reads[%d].Token", i)
			}
		}
	}
	if job.AgentToken != nil {
		return "AgentToken"
	}
	if job.API != nil {
		return "API"
	}
	return ""
}

// execute runs the steps up to the validated Result.
func (r *run) execute(ctx context.Context) stepResult {
	// Before any step: a gateway-mode job holds no credential (workspace-run.md §Gateway mode).
	if r.p.o.GitAuth == GitAuthGateway {
		if f := jobCredential(r.job); f != "" {
			return failed(runner.ReasonHarnessError, &GatewayCredentialError{Field: f})
		}
	}
	h := r.task.Spec.Harness
	if h == nil || h.Resolved == nil || h.Resolved.Name == "" {
		return failed(runner.ReasonHarnessError, errors.New("the task has no resolved harness"))
	}
	ad, ok := r.p.o.Adapters[h.Resolved.Name]
	if !ok {
		return failed(runner.ReasonHarnessError, fmt.Errorf("no adapter for harness %q", h.Resolved.Name))
	}
	r.adapter = ad

	type step struct {
		name   string
		reason string
		fn     func(context.Context) error
	}
	steps := []step{
		{"workspace", runner.ReasonHarnessError, r.makeWorkspace},
		{"credentials", runner.ReasonHarnessError, r.startCredentials},
		{"clone", runner.ReasonHarnessError, r.cloneRepos},
	}
	if needsEnvironment(r.task) {
		steps = append(steps, step{"environment", runner.ReasonEnvironmentError, r.environmentUp})
	}
	steps = append(steps,
		step{"skills", runner.ReasonHarnessError, r.materialiseSkills},
		step{"schema", runner.ReasonHarnessError, r.prepareSchema},
		step{"prompt", runner.ReasonHarnessError, r.renderPrompt},
		step{"env", runner.ReasonHarnessError, r.buildEnv},
	)
	for _, s := range steps {
		t0 := r.p.clk.Now()
		err := s.fn(ctx)
		r.stepDone(s.name, t0, err)
		if err != nil {
			return failed(s.reason, fmt.Errorf("%s: %w", s.name, err))
		}
		r.log.Debug("pipeline step done", "step", s.name)
	}
	t0 := r.p.clk.Now()
	err := ad.Prepare(ctx, r.ws, r.task)
	r.stepDone("prepare", t0, err)
	if err != nil {
		return failed(runner.ReasonHarnessError, fmt.Errorf("prepare %s: %w", ad.Name(), err))
	}
	return r.runHarness(ctx)
}

// stepDone reports a step that started at t0 to Options.OnStep, its error redacted.
func (r *run) stepDone(name string, t0 time.Time, err error) {
	if f := r.p.o.OnStep; f != nil {
		f(name, r.p.clk.Now().Sub(t0), r.red.Error(err))
	}
}

// runHarness runs the harness, validates the Result and, once, repairs it.
func (r *run) runHarness(ctx context.Context) stepResult {
	r.harnessed = true
	r.mon.countTurns(true)
	r.log.Info("harness starting", "harness", r.adapter.Name())
	t0 := r.p.clk.Now()
	raw, err := r.adapter.Run(ctx, r.ws, r.task, r.prompt, r.mon)
	r.parts = append(r.parts, raw.Transcript)
	r.mon.countTurns(false)
	if res, stop := r.harnessEnded(ctx, err); stop {
		r.stepDone("harness", t0, res.err)
		return res
	}
	res, problems := r.checkResult(raw)
	r.stepDone("harness", t0, nil)
	if len(problems) == 0 {
		r.result = res
		return stepResult{status: string(res.Status)}
	}
	r.log.Info("the result is invalid: one repair turn", "problems", len(problems))
	r.parts = append(r.parts, factoryLine("repair", map[string]any{"problems": problems}))
	t0 = r.p.clk.Now()
	if err := os.Remove(r.ws.Result); err != nil && !errors.Is(err, os.ErrNotExist) {
		s := failed(runner.ReasonHarnessError, fmt.Errorf("remove the invalid result: %w", err))
		r.stepDone("repair", t0, s.err)
		return s
	}
	raw, err = r.adapter.Repair(ctx, r.ws, problems, r.mon)
	r.parts = append(r.parts, raw.Transcript)
	if res, stop := r.harnessEnded(ctx, err); stop {
		r.stepDone("repair", t0, res.err)
		return res
	}
	res, problems = r.checkResult(raw)
	if len(problems) > 0 {
		r.parts = append(r.parts, factoryLine("invalid-result", map[string]any{"problems": problems}))
		s := failed(runner.ReasonInvalidResult, fmt.Errorf("still invalid after the repair turn: %s", strings.Join(problems, "; ")))
		r.stepDone("repair", t0, s.err)
		return s
	}
	r.stepDone("repair", t0, nil)
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

// finish turns the last step into the Outcome, storing the transcript when the harness ran,
// and hands it to Options.Finished. parent is the worker's context.
func (r *run) finish(parent context.Context, s stepResult) runner.Outcome {
	t0 := r.p.clk.Now()
	var storeErr error
	// A step that failed because a limit or a rate limit cancelled it (the clone was
	// cancelled by the wall-clock timer) ended for that reason.
	if cause := r.mon.stopped(); cause != nil && s.status == runner.StatusFailed &&
		(s.reason == runner.ReasonHarnessError ||
			s.reason == runner.ReasonEnvironmentError || s.reason == runner.ReasonCancelled) {
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
		res := FailedResult(r.id, s.reason, s.err, r.red.String)
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
		ref, err := r.storeTranscript(uctx)
		cancel()
		if err != nil {
			storeErr = err
			r.log.Warn("transcript not uploaded", "err", r.red.String(err.Error()))
		} else {
			out.TranscriptRef = ref
		}
	}
	r.stepDone("finish", t0, storeErr)
	if f := r.p.o.Finished; f != nil {
		f(out)
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

// cleanup always runs: the environment down, the credential socket closed, the
// workspace removed (run-pipeline.md §Steps 12), except a Root, which is kept for whoever
// collects out/.
func (r *run) cleanup(parent context.Context) {
	if r.envUp {
		// The Environment owns its budget (envrun's DownTimeout, which vmrun shortens after a
		// signal): no deadline here.
		if err := r.p.o.Environment.Down(context.WithoutCancel(parent)); err != nil {
			r.log.Warn("environment down failed", "err", r.red.String(err.Error()))
		}
	}
	if r.creds != nil {
		if err := r.creds.Close(); err != nil {
			r.log.Warn("closing the credential socket failed", "err", err)
		}
		r.creds = nil
	}
	if r.ws.Root == "" || r.p.o.Root != "" {
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

// FailedResult is the failed Result of a run that ended without one of its own, the pipeline's
// and the VM's (workspace-run.md, decided 2026-10-22): its summary is reason, then ": " and
// err's message redacted by redact (nil: by the token shapes alone, RedactShapes) when err
// isn't nil, cut to 500 bytes on a rune boundary.
func FailedResult(task, reason string, err error, redact func(string) string) v1alpha1.Result {
	if redact == nil {
		redact = RedactShapes
	}
	summary := reason
	if err != nil {
		summary += ": " + redact(err.Error())
	}
	return v1alpha1.Result{Task: task, Status: v1alpha1.ResultFailed, Summary: truncate(summary, maxSummary)}
}

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
