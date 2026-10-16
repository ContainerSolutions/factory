package v1alpha1

// ResultStatus is how a run ended.
type ResultStatus string

// Result statuses.
const (
	ResultDone       ResultStatus = "done"
	ResultFailed     ResultStatus = "failed"
	ResultNeedsInput ResultStatus = "needs_input"
	ResultNoop       ResultStatus = "noop"
)

// Result is what an agent reports when a run completes. Spec-specific fields go under Data
// and are validated by the TaskSpec's output schema.
type Result struct {
	Task      string         `json:"task"`
	Status    ResultStatus   `json:"status" jsonschema:"enum=done,enum=failed,enum=needs_input,enum=noop"`
	Outcome   string         `json:"outcome,omitzero"`
	Summary   string         `json:"summary,omitzero"`
	Verdict   map[string]any `json:"verdict,omitzero"`
	Artifacts []Artifact     `json:"artifacts,omitzero"`
	Suggest   []Suggestion   `json:"suggest,omitzero"`
	// Question is set when Status is needs_input; null otherwise.
	Question *string        `json:"question" jsonschema:"nullable"`
	Harness  *ResultHarness `json:"harness,omitzero"`
	Data     map[string]any `json:"data,omitzero"`
}

// Artifact is something the run produced.
type Artifact struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitzero"`
	Ref  string `json:"ref,omitzero"`
}

// Suggestion is a follow-up the agent proposes; rules decide.
type Suggestion struct {
	TaskSpec string         `json:"taskSpec"`
	Reason   string         `json:"reason,omitzero"`
	Inputs   map[string]any `json:"inputs,omitzero"`
}

// ResultHarness is the harness that ran, its effective config and usage.
type ResultHarness struct {
	Name            string          `json:"name"`
	Version         string          `json:"version,omitzero"`
	Auth            string          `json:"auth,omitzero"`
	EffectiveConfig EffectiveConfig `json:"effectiveConfig"`
	Usage           Usage           `json:"usage"`
}

// EffectiveConfig is the exact config the harness ran with.
type EffectiveConfig struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitzero"`
}

// Usage is what a run consumed. CostUSD is null for subscription runs.
type Usage struct {
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	Turns        int      `json:"turns,omitzero"`
	WallClock    Duration `json:"wallClock,omitzero"`
	CostUSD      *float64 `json:"costUSD" jsonschema:"nullable"`
}

// ---- Capability descriptors ----------------------------------------------------------------

// AdapterCapabilities is published by each harness adapter (static per adapter version).
// `factory validate --capabilities` checks catalog patterns against it.
type AdapterCapabilities struct {
	Harness string `json:"harness"`
	// EffortMapping maps a normalised effort to the harness's native value.
	EffortMapping map[string]string `json:"effortMapping,omitzero"`
	// Features: "structured-output", "mcp", "plugins", "skills-dir".
	Features []string `json:"features,omitzero"`
}

// HarnessOffer is what one agent can actually run with one harness (from the agent's Probe
// and local config): concrete values, not patterns.
type HarnessOffer struct {
	Name    string `json:"name"`
	Version string `json:"version,omitzero"`
	// Auth is subscription, apiKey, accessToken or cloud.
	Auth   string   `json:"auth,omitzero"`
	Models []string `json:"models,omitzero"`
	// DefaultModel is the model the harness uses when none is asked for; resolution prefers
	// it over the highest version (patterns LLD). Empty when the probe can't tell.
	DefaultModel    string   `json:"defaultModel,omitzero"`
	ReasoningEffort []string `json:"reasoningEffort,omitzero"`
}
