package vmrun

import (
	"log/slog"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// events writes the run's event stream (workspace-run.md §Event stream) to stdout: it is the
// pipeline's runner.Sink (progress and usage) and its OnStep (step), and sends start and
// outcome. Safe for concurrent use (the Encoder serialises lines). An event that can't be
// written is logged and dropped: the outcome is in outcome.json anyway.
type events struct {
	enc *runbundle.Encoder
	log *slog.Logger
}

var _ runner.Sink = (*events)(nil)

func (e *events) send(ev runbundle.Event) {
	if err := e.enc.Encode(ev); err != nil {
		e.log.Warn("an event wasn't sent", "type", ev.Type, "err", err)
	}
}

// start is the first event, once the bundle is read.
func (e *events) start(m runbundle.Manifest) {
	e.send(runbundle.Event{Type: runbundle.EventStart, Start: &runbundle.Start{Version: m.Version, RunID: m.RunID, TaskID: m.TaskID}})
}

// Progress implements runner.Sink. The pipeline has redacted and bounded the message.
func (e *events) Progress(p v1alpha1.ProgressEvent) {
	e.send(runbundle.Event{Type: runbundle.EventProgress, Progress: &p})
}

// Usage implements runner.Sink.
func (e *events) Usage(u runner.UsageDelta) {
	e.send(runbundle.Event{Type: runbundle.EventUsage, Usage: runbundle.UsageOf(u)})
}

// step is pipeline.Options.OnStep: err is already redacted with the run's redactor.
func (e *events) step(name string, d time.Duration, err error) {
	e.send(runbundle.Event{Type: runbundle.EventStep, Step: runbundle.StepOf(name, d, err)})
}

// outcome is the outcome event: o without its Result, which is in outcome.json.
func (e *events) outcome(o runbundle.Outcome) {
	e.send(runbundle.OutcomeEvent(o))
}
