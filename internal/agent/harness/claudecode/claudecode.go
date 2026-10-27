// Package claudecode is the Claude Code CLI harness adapter (design/workstreams/agent/LLD/
// harness-adapters.md §Claude Code). It runs only in a VM workspace (`factory-agent run`,
// harness.IsolationVM; workspace-run.md §Harnesses under IsolationVM), where the VM is the
// boundary.
//
// Prepare writes a settings file (no marketplace plugins, the reasoning effort, and a
// PreToolUse hook that refuses `git push` and gh writes and keeps the file tools' writes in
// the run's writable directories) and an MCP config with the Effector, both in a scratch
// directory of the workspace's HOME, and makes the scratch CLAUDE_CONFIG_DIR; the skills are
// the pipeline's plugin directories, passed as they are. Run starts `claude -p
// --output-format stream-json` with the resolved model, the headless preamble appended to the
// system prompt and the Result schema, with harness.Placeholder as the credential (the gateway
// outside the VM replaces it with the real one), streams its events into the EventSink, stops
// on a rate or plan limit, and kills the whole process group when its context ends. Repair
// resumes the same session with the validation problems.
//
// Every claude flag, file, settings key and event name the adapter relies on is in flags.go,
// the unchecked ones marked UNVERIFIED until a VM workspace run exercises them.
package claudecode

import (
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Name is the harness's name.
const Name = config.HarnessClaudeCode

// Auth modes in a HarnessOffer.
const (
	AuthSubscription = "subscription"
	AuthAPIKey       = "apiKey"
)

// DefaultKillGrace is how long claude gets to exit after SIGTERM before its process group is
// killed.
const DefaultKillGrace = 5 * time.Second

// DefaultThrottle is how long a rate-limited agent waits when the error gives no reset
// (harness-adapters.md: else +15 min).
const DefaultThrottle = clirun.DefaultThrottle

// Options configure an Adapter. Isolation must be IsolationVM.
type Options struct {
	// Config is the claude-code harness's config: vmrun makes it from the task's resolved
	// harness (Default is the model when the task names none).
	Config config.Harness
	// Binary is the claude executable. Default: "claude" on PATH.
	Binary string
	// Clock stamps events and rate-limit resets. Default: clock.Real.
	Clock clock.Clock
	// KillGrace is the SIGTERM-to-SIGKILL delay on cancel. Default: DefaultKillGrace.
	KillGrace time.Duration
	// Throttle is the wait on a rate limit without a reset. Default: DefaultThrottle.
	Throttle time.Duration
	// Isolation is what bounds the run's commands: IsolationVM, a VM workspace
	// (workspace-run.md §Harnesses under IsolationVM). Any other value, the zero one (the host,
	// which phase 10 removed) included, is refused by every method.
	Isolation harness.Isolation
	// Getuid is the run's user id: a root run sets IS_SANDBOX. Default: os.Getuid.
	Getuid func() int
}

// Adapter is the claude-code harness adapter. It keeps no per-run state in memory (the
// session to resume is in the workspace), so one Adapter serves concurrent runs.
type Adapter struct {
	o Options
}

var _ harness.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = "claude"
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
	if o.Getuid == nil {
		o.Getuid = os.Getuid
	}
	return &Adapter{o: o}
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return Name }

// Capabilities implements harness.Adapter: the effort mapping and claude's features.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{
		Harness:       Name,
		EffortMapping: maps.Clone(effortMapping),
		Features:      []string{"structured-output", "mcp", "plugin-dir", "pre-tool-use-hook"},
	}
}

// checkVM refuses an Isolation other than IsolationVM: claude runs only in a VM workspace,
// never on the host without its sandbox.
func (a *Adapter) checkVM() error {
	if a.o.Isolation != harness.IsolationVM {
		return fmt.Errorf("claude-code: runs only in a VM workspace, not under isolation %s", a.o.Isolation)
	}
	return nil
}
