// Package runner is the seam between the worker's lease loop and what executes a leased task:
// the worker hands a Runner each Job and reports the Outcome to the dispatcher. The run
// pipeline (AGT-S1-T02, run-pipeline.md) implements Runner; Stub and Func stand in for it in
// tests and development.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Runner executes one leased task. Run returns when the task is done, failed or throttled, or
// once ctx is cancelled (the dispatcher cancelled the task, the lease is gone, or the worker is
// shutting down); the worker decides what to report from the Outcome and why ctx ended. An
// error means the run broke before it produced an Outcome (reported as failed{harness-error}).
type Runner interface {
	Run(ctx context.Context, job *Job, sink Sink) (Outcome, error)
}

// Func adapts a function to Runner.
type Func func(ctx context.Context, job *Job, sink Sink) (Outcome, error)

// Run calls f.
func (f Func) Run(ctx context.Context, job *Job, sink Sink) (Outcome, error) {
	return f(ctx, job, sink)
}

// Job is one leased task, as the worker hands it to a Runner.
type Job struct {
	// Lease is the dispatcher's lease response, passed through unchanged: the lease, the Task
	// and the git credential it came with (plus whatever the contract adds later).
	Lease apiclient.Lease
	// Git is the task's current git credential: the lease's, swapped when a heartbeat
	// returns a refreshed one. The credential helper answers from it.
	Git *Credentials
	// API is the dispatcher client, authenticated as the agent (prompt, context, transcript
	// upload, effects).
	API *apiclient.ClientWithResponses
	// Dispatcher is the dispatcher's base URL (FACTORY_API for the harness).
	Dispatcher string
	// AgentToken returns the agent's current token (FACTORY_TOKEN for the harness). Never log
	// it.
	AgentToken func() string
	// StartedAt is when the worker started the job (the drain deadline counts from it).
	StartedAt time.Time
}

// TaskID is the leased task's id.
func (j *Job) TaskID() string { return j.Lease.Task.Metadata.ID }

// Sink takes what a run reports while it runs; the worker batches it into heartbeats.
type Sink interface {
	// Progress is one progress line (a summary; full detail goes to the transcript).
	Progress(v1alpha1.ProgressEvent)
	// Usage is token usage since the previous call (a delta, not a total).
	Usage(UsageDelta)
}

// UsageDelta is usage since the previous report. CostUSD is nil for subscription runs.
type UsageDelta struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      *float64
}

// Outcome statuses (run-pipeline.md §Outcome to the worker): the Result statuses plus
// throttled, which is no Result at all (the task goes back to Pending).
const (
	StatusDone       = string(v1alpha1.ResultDone)
	StatusFailed     = string(v1alpha1.ResultFailed)
	StatusNeedsInput = string(v1alpha1.ResultNeedsInput)
	StatusNoop       = string(v1alpha1.ResultNoop)
	StatusThrottled  = "throttled"
)

// Outcome reasons.
const (
	ReasonLimit         = "limit"
	ReasonInvalidResult = "invalid-result"
	ReasonHarnessError  = "harness-error"
	ReasonStackError    = "stack-error"
	ReasonCancelled     = "cancelled"
)

// Outcome is what a run ended with (run-pipeline.md).
type Outcome struct {
	// Result is the run's Result; nil when the run failed before producing one.
	Result *v1alpha1.Result
	// Status is done, failed, needs_input, noop or throttled.
	Status string
	// Reason qualifies a failure: limit, invalid-result, harness-error, stack-error,
	// cancelled.
	Reason string
	// ThrottledUntil is when the harness's limit resets (Status throttled); nil when unknown.
	ThrottledUntil *time.Time
	// TranscriptRef is the uploaded transcript's reference, if any.
	TranscriptRef string
}

// Credentials holds a task's current git credential, safe for concurrent use. Its String,
// Format and LogValue never print the token.
type Credentials struct {
	mu sync.RWMutex
	c  apiclient.GitCredential
}

// NewCredentials holds c.
func NewCredentials(c apiclient.GitCredential) *Credentials { return &Credentials{c: c} }

// Get returns the current credential.
func (c *Credentials) Get() apiclient.GitCredential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.c
}

// Set swaps in a refreshed credential.
func (c *Credentials) Set(n apiclient.GitCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.c = n
}

// String describes the credential without its token.
func (c *Credentials) String() string {
	g := c.Get()
	return fmt.Sprintf("git credential for %s on %s, expires %s, token [REDACTED]", g.Repo, g.Host, g.ExpiresAt.Format(time.RFC3339))
}

// Format prints String for every verb, so %v, %+v and %#v can't leak the token.
func (c *Credentials) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, c.String()) }

// LogValue keeps the token out of slog.
func (c *Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON keeps the token out of JSON dumps.
func (c *Credentials) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
