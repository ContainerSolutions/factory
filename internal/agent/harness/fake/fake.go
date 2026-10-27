// Package fake is a scriptable harness.Adapter for pipeline and worker tests
// (harness-adapters.md §Fake adapter): scripted progress, usage and results, a scripted
// repair turn, and ways to simulate a rate limit or a run that only ends when it's stopped.
package fake

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// Name is the fake harness's default name.
const Name = "fake"

// Turn is one scripted Run or Repair.
type Turn struct {
	// Events and Usage are reported to the sink in order (Usage after Events).
	Events []harness.ProgressEvent
	Usage  []harness.UsageDelta
	// Result, when non-nil, is written to the workspace's $FACTORY_RESULT file.
	Result []byte
	// Output is returned as Raw.Result (the harness's structured output).
	Output []byte
	// Transcript lines are returned as Raw.Transcript.
	Transcript []string
	// Err is returned after the turn.
	Err error
	// RateLimitAfter > 0 reports RateLimited(RateLimitUntil) after that many events and
	// then waits for ctx to end.
	RateLimitAfter int
	RateLimitUntil time.Time
	// Block waits for ctx to end after the events (a run only a limit stops).
	Block bool
	// Do runs before the events, with the workspace and the prompt ("" for a repair); an
	// error ends the turn with it.
	Do func(ctx context.Context, ws harness.Workspace, prompt string) error
}

// Script is the whole fake run: the Run turn and the repair turns, in order.
type Script struct {
	Turn
	// PrepareErr is returned by Prepare.
	PrepareErr error
	// Repairs are the successive Repair turns; a Repair beyond them writes nothing.
	Repairs []Turn
}

// Adapter is the fake harness. It records what it was given; safe for concurrent use.
type Adapter struct {
	// HarnessName is Name() (default "fake").
	HarnessName string
	Script      Script

	mu       sync.Mutex
	prepared []harness.Workspace
	prompts  []string
	problems [][]string
	repairs  int
}

var _ harness.Adapter = (*Adapter)(nil)

// Name implements harness.Adapter.
func (a *Adapter) Name() string {
	if a.HarnessName == "" {
		return Name
	}
	return a.HarnessName
}

// Capabilities implements harness.Adapter.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{Harness: a.Name(), Features: []string{"structured-output", "skills-dir"}}
}

// Prepare implements harness.Adapter.
func (a *Adapter) Prepare(_ context.Context, ws harness.Workspace, _ v1alpha1.Task) error {
	a.mu.Lock()
	a.prepared = append(a.prepared, ws)
	a.mu.Unlock()
	return a.Script.PrepareErr
}

// Run implements harness.Adapter.
func (a *Adapter) Run(ctx context.Context, ws harness.Workspace, _ v1alpha1.Task, prompt string, sink harness.EventSink) (harness.Raw, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, prompt)
	a.mu.Unlock()
	return play(ctx, ws, a.Script.Turn, prompt, sink)
}

// Repair implements harness.Adapter.
func (a *Adapter) Repair(ctx context.Context, ws harness.Workspace, problems []string, sink harness.EventSink) (harness.Raw, error) {
	a.mu.Lock()
	a.problems = append(a.problems, slices.Clone(problems))
	n := a.repairs
	a.repairs++
	a.mu.Unlock()
	if n >= len(a.Script.Repairs) {
		return harness.Raw{}, nil
	}
	return play(ctx, ws, a.Script.Repairs[n], "", sink)
}

// Prepared returns the workspaces Prepare was given.
func (a *Adapter) Prepared() []harness.Workspace {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.prepared)
}

// Prompts returns the prompts Run was given.
func (a *Adapter) Prompts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.prompts)
}

// Problems returns the problems each Repair was given.
func (a *Adapter) Problems() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.problems)
}

func play(ctx context.Context, ws harness.Workspace, t Turn, prompt string, sink harness.EventSink) (harness.Raw, error) {
	raw := harness.Raw{Result: t.Output}
	if len(t.Transcript) > 0 {
		raw.Transcript = []byte(strings.Join(t.Transcript, "\n") + "\n")
	}
	if t.Do != nil {
		if err := t.Do(ctx, ws, prompt); err != nil {
			return raw, err
		}
	}
	for i, e := range t.Events {
		if err := ctx.Err(); err != nil {
			return raw, err
		}
		if t.RateLimitAfter > 0 && i == t.RateLimitAfter {
			sink.RateLimited(t.RateLimitUntil)
			<-ctx.Done()
			return raw, ctx.Err()
		}
		sink.Progress(e)
	}
	if t.RateLimitAfter > 0 && t.RateLimitAfter >= len(t.Events) {
		sink.RateLimited(t.RateLimitUntil)
		<-ctx.Done()
		return raw, ctx.Err()
	}
	for _, u := range t.Usage {
		sink.Usage(u)
	}
	if t.Block {
		<-ctx.Done()
		return raw, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return raw, err
	}
	if t.Result != nil {
		if err := os.WriteFile(ws.Result, t.Result, 0o600); err != nil {
			return raw, err
		}
	}
	return raw, t.Err
}
