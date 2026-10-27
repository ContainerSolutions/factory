// Package harness is the seam between the run pipeline and the coding-agent CLIs it drives
// (design/workstreams/agent/LLD/harness-adapters.md): the Adapter interface each harness
// implements (Codex in AGT-S1-T03, Claude Code in AGT-S1-T04, the scriptable fake in
// harness/fake), the Workspace the pipeline prepares for it, and the EventSink it reports
// progress, usage and rate limits to.
package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Isolation is what bounds a run's commands (workspace-run.md §Harnesses under
// IsolationVM): an adapter option (claudecode.Options.Isolation, codex.Options.Isolation).
type Isolation int

const (
	// IsolationSandbox, the zero value, was the owner's host, where the harness's own OS
	// sandbox bounded every command. Phase 10 removed the host runtime: the adapters refuse it.
	IsolationSandbox Isolation = iota
	// IsolationVM is `factory-agent run` inside a VM workspace: the VM is the boundary, so
	// the harness runs without its own sandbox, and it holds no credential: the gateway
	// outside the VM injects every one, and the harness gets Placeholder instead.
	IsolationVM
)

func (i Isolation) String() string {
	switch i {
	case IsolationSandbox:
		return "sandbox"
	case IsolationVM:
		return "vm"
	}
	return fmt.Sprintf("Isolation(%d)", int(i))
}

// Placeholder is the one placeholder credential a VM workspace carries where a tool refuses
// to start without one (CLAUDE_CODE_OAUTH_TOKEN, GH_TOKEN, codex's auth.json). It matches
// none of the transcript redactor's token patterns, and the gateway replaces whatever value
// it finds, so it never has to recognise this one.
const Placeholder = "factory-placeholder-not-a-credential"

// Adapter drives one harness. The pipeline calls Prepare once, then Run, then at most one
// Repair when the Result was invalid. Run and Repair return when the harness exits or ctx is
// done; the pipeline cancels ctx on a limit (wall-clock, turns, cost) or a rate limit.
type Adapter interface {
	// Name is the harness's name: codex, claude-code.
	Name() string
	// Capabilities is static per adapter version: the effort mapping and features.
	Capabilities() v1alpha1.AdapterCapabilities
	// Prepare lays out the harness's own config in the workspace: the scratch config dir,
	// the skills in its layout, the MCP wiring.
	Prepare(ctx context.Context, ws Workspace, t v1alpha1.Task) error
	// Run runs the harness on prompt with t's resolved harness config and limits.
	Run(ctx context.Context, ws Workspace, t v1alpha1.Task, prompt string, sink EventSink) (Raw, error)
	// Repair asks the harness, in the same session where it can, to emit a valid Result only:
	// problems are the validation errors of the previous one.
	Repair(ctx context.Context, ws Workspace, problems []string, sink EventSink) (Raw, error)
}

// EventSink takes what a harness reports while it runs. Implementations are safe for
// concurrent use.
type EventSink interface {
	// Progress is one progress line.
	Progress(ProgressEvent)
	// Usage is usage since the previous call (a delta).
	Usage(UsageDelta)
	// RateLimited says the harness's plan limit is hit until the given time (zero when
	// unknown): the pipeline stops the run and hands the task back as throttled.
	RateLimited(until time.Time)
}

// Progress event kinds.
const (
	KindTurn    = "turn"
	KindTool    = "tool"
	KindMessage = "message"
	KindError   = "error"
)

// MaxSummary bounds a ProgressEvent's summary; full detail goes to the transcript.
const MaxSummary = 500

// ProgressEvent is one normalised progress line. Each KindTurn event counts one turn
// against limits.maxTurns.
type ProgressEvent struct {
	Kind    string
	Summary string
	At      time.Time
}

// UsageDelta is usage since the previous report. CostUSD is nil for subscription runs.
type UsageDelta struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      *float64
}

// Raw is what one Run or Repair produced.
type Raw struct {
	// Result is the harness's structured output, when it has one. The pipeline prefers the
	// file at Workspace.Result ($FACTORY_RESULT) when the run wrote it.
	Result []byte
	// Transcript is the harness's stream log as JSON Lines (one JSON value per line); lines
	// that aren't JSON are kept as text. It is redacted before upload.
	Transcript []byte
}

// LimitError is returned by Run or Repair when the harness itself stopped on a limit (its
// own --max-turns, a budget flag).
type LimitError struct {
	// Limit is maxTurns, maxWallClock or maxCostUSD.
	Limit  string
	Detail string
}

func (e *LimitError) Error() string {
	if e.Detail == "" {
		return "limit " + e.Limit + " reached"
	}
	return fmt.Sprintf("limit %s reached: %s", e.Limit, e.Detail)
}

// Limit names.
const (
	LimitTurns     = "maxTurns"
	LimitWallClock = "maxWallClock"
	LimitCost      = "maxCostUSD"
)

// IsLimit reports whether err is (or wraps) a *LimitError.
func IsLimit(err error) bool {
	var le *LimitError
	return errors.As(err, &le)
}

// Workspace is the scratch directory the pipeline prepared for one run
// (run-pipeline.md §Steps 1): every path is absolute and inside Root.
type Workspace struct {
	// Root is workRoot/<taskID>, mode 0700.
	Root string
	// Repo is the primary checkout (repo/): the rw repo, or the task repo read-only. Empty
	// when the task checks nothing out.
	Repo string
	// RepoWritable is whether the harness may write Repo (a rw mount, or a checkout other
	// than read-only). Adapters pick their sandbox from it.
	RepoWritable bool
	// Mounts are every checked-out repo, the primary first.
	Mounts []Mount
	// WorkDir is where the harness runs: Repo, else the first mount, else Root.
	WorkDir string
	// Skills is skills/: the selected ccf-skills, materialised as plugin directories
	// (<plugin>/.claude-plugin/plugin.json, the plugin's other root files such as bin/ and
	// rules/, and <plugin>/skills/<skill>/SKILL.md for the selected skills only). Each
	// plugin's bin/ is on Env's PATH, after Bin.
	Skills string
	// SkillList is every materialised skill.
	SkillList []Skill
	// Out is out/ ($FACTORY_OUT); Result is out/result.json ($FACTORY_RESULT).
	Out    string
	Result string
	// Schema is the JSON Schema file the Result must match (for --output-schema and
	// --json-schema).
	Schema string
	// Home is the harness's scratch HOME; Tmp its TMPDIR; Bin holds the `factory` CLI and
	// is first on PATH.
	Home string
	Tmp  string
	Bin  string
	// Env is the harness's whole environment (KEY=value), already reduced to the
	// allow-list: adapters append their own (CODEX_HOME, CLAUDE_CONFIG_DIR) and nothing from
	// the owner's environment.
	Env []string
	// Preamble is the headless preamble, also prepended to the prompt; adapters may put it
	// in the system prompt (AGENTS.md, --append-system-prompt).
	Preamble string
	// Hosts are the hosts the run's commands must reach: the git host its repos come from,
	// the dispatcher's, the local stack's when it is up, and the Task's spec.hosts (an
	// adapter's network sandbox allows them).
	Hosts []string
}

// Mount is one checked-out repo.
type Mount struct {
	// Repo is owner/name; Ref the branch, tag or commit asked for ("" = default branch).
	Repo string
	Ref  string
	// Dir is the checkout.
	Dir      string
	Writable bool
}

// Skill is one materialised skill.
type Skill struct {
	// Name is the skill's folder name; Plugin the plugin it belongs to.
	Name   string
	Plugin string
	// Dir is skills/<plugin>/skills/<name> (it holds SKILL.md); the plugin's root is
	// skills/<plugin>.
	Dir string
}
