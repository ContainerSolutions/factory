// Package codex is the Codex CLI harness adapter (design/workstreams/agent/LLD/
// harness-adapters.md §Codex). It runs only in a VM workspace (`factory-agent run`,
// workspace-run.md §Harnesses in the VM), where the VM is the boundary.
//
// Prepare lays out a scratch CODEX_HOME in the workspace: a placeholder auth.json
// (placeholderAuth: the account's identifiers in an unsigned id_token, harness.Placeholder as
// the tokens, and a fresh last_refresh, so codex never refreshes), a generated config.toml
// (no update check, the run's environment for commands, the Effector's MCP server), AGENTS.md
// with the headless preamble, and the task's skills flattened from the pipeline's plugin
// layout. Run starts `codex exec --json` with the resolved model and effort, without codex's
// own sandbox (`--sandbox danger-full-access`), from the pipeline's WorkDir, streams its JSONL
// into the EventSink, stops on a rate or plan limit, and kills the whole process group when
// its context ends. Repair resumes the same session with the validation problems. The gateway
// outside the VM replaces the bearer and the account header with the owner's, and refreshes
// the real token itself.
//
// Every codex flag, config key and event name the adapter relies on is in flags.go, each
// marked UNVERIFIED until a VM workspace run exercises it.
package codex

import (
	"maps"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// Name is the harness's name.
const Name = config.HarnessCodex

// Auth modes in a HarnessOffer.
const (
	AuthSubscription = "subscription"
	AuthAPIKey       = "apiKey"
)

// DefaultThrottle is how long a rate-limited agent waits when the error gives no reset
// (harness-adapters.md: else +15 min).
const DefaultThrottle = clirun.DefaultThrottle

// DefaultKillGrace is how long codex gets to exit after SIGTERM before its process group is
// killed.
const DefaultKillGrace = 5 * time.Second

// Options configure an Adapter. Account is required.
type Options struct {
	// Config is the codex harness's config: vmrun makes it from the task's resolved harness
	// (Default is the model when the task names none).
	Config config.Harness
	// Binary is the codex executable. Default: "codex" on PATH.
	Binary string
	// Clock stamps events and rate-limit resets. Default: clock.Real.
	Clock clock.Clock
	// KillGrace is the SIGTERM-to-SIGKILL delay on cancel. Default: DefaultKillGrace.
	KillGrace time.Duration
	// Throttle is the wait on a rate limit without a reset. Default: DefaultThrottle.
	Throttle time.Duration
	// Account is the ChatGPT account codex runs as: the run bundle's codex/account.json,
	// identifiers only (P1 §5). Its id_token claims pick codex's workspace, which fails
	// without the real ones (Phase 0, Q5). Required.
	Account *runbundle.CodexAccount
}

// Adapter is the codex harness adapter. It keeps no per-run state in memory (the session to
// resume is in the workspace), so one Adapter serves concurrent runs.
type Adapter struct {
	o Options
}

var _ harness.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = "codex"
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.KillGrace <= 0 {
		o.KillGrace = DefaultKillGrace
	}
	if o.Throttle <= 0 {
		o.Throttle = DefaultThrottle
	}
	return &Adapter{o: o}
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return Name }

// Capabilities implements harness.Adapter: the LLD's effort mapping (max → high, xhigh for
// the models that take it) and codex's features.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{
		Harness:       Name,
		EffortMapping: maps.Clone(effortMapping),
		Features:      []string{"structured-output", "mcp", "skills-dir"},
	}
}
