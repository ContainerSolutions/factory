// Package opencode is the OpenCode harness adapter (design/workstreams/agent/LLD/
// harness-adapters.md §OpenCode, ADR-0010): a model the owner serves behind the gateway's
// model endpoint, run only in a VM workspace, where the VM is the boundary. Prepare writes
// opencode.json (one provider at the gateway, with harness.Placeholder as its key, which the
// gateway replaces) and the flattened skills under the scratch XDG_CONFIG_HOME. Run starts
// `opencode run --format json` on the prompt, streams its JSON lines into the EventSink, stops
// on a rate limit or the model server being unreachable, and kills the whole process group
// when its context ends; Repair resumes the session with the validation problems. Every
// opencode name the adapter relies on is in flags.go.
package opencode

import (
	"maps"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// Name is the harness's name.
const Name = config.HarnessOpenCode

// AuthSelfHosted is opencode's auth mode in a HarnessOffer: the model server's API key, which
// the agent host holds and the gateway injects.
const AuthSelfHosted = "selfHosted"

// DefaultThrottle is how long a rate-limited agent waits (harness-adapters.md: 429,
// overloaded or 503 → now+2m) when the error gives no reset.
const DefaultThrottle = 2 * time.Minute

// OutageThrottle is how long the agent waits when the gateway can't reach the model server
// (its upstream or timeout answer: the server or the tunnel is down).
const OutageThrottle = 5 * time.Minute

// DefaultKillGrace is how long opencode gets to exit after SIGTERM before its process group
// is killed.
const DefaultKillGrace = 5 * time.Second

// Options configure an Adapter.
type Options struct {
	// Config is the opencode harness's config (Default is the model when the task names none).
	Config config.Harness
	// Binary is the opencode executable. Default: "opencode" on PATH.
	Binary string
	// Clock stamps events and rate-limit resets. Default: clock.Real.
	Clock clock.Clock
	// KillGrace is the SIGTERM-to-SIGKILL delay on cancel. Default: DefaultKillGrace.
	KillGrace time.Duration
	// Throttle is the wait on a rate limit without a reset. Default: DefaultThrottle.
	Throttle time.Duration
	// ContextWindow and MaxOutput are the served model's limits in tokens, from the run
	// bundle's Model (the owner's agent config). Prepare refuses a run without them.
	ContextWindow int
	MaxOutput     int
}

// Adapter is the opencode harness adapter. It keeps no per-run state in memory, so one
// Adapter serves concurrent runs.
type Adapter struct {
	o Options
}

var _ harness.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = "opencode"
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

// Capabilities implements harness.Adapter. structured-output is the Result through
// $FACTORY_RESULT, which the pipeline validates and repairs.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{
		Harness:       Name,
		EffortMapping: maps.Clone(effortMapping),
		Features:      []string{"structured-output", "mcp", "skills-dir"},
	}
}
