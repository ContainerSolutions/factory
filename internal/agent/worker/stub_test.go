package worker_test

import (
	"context"
	"sync"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// stubRunner is a runner.Runner that reports canned progress and usage, optionally waits,
// and returns a canned Outcome. It is safe for concurrent use.
type stubRunner struct {
	// Outcome is returned for every job; its Result (if any) is copied with Task set to the
	// job's task. The zero Outcome is done with an empty Result.
	Outcome runner.Outcome
	// Err, when set, is returned instead of the Outcome.
	Err error
	// Progress and Usage are reported to the sink before the run ends.
	Progress []v1alpha1.ProgressEvent
	Usage    *runner.UsageDelta
	// Hold, when set, is waited on before returning: the run ends when Hold is closed (with
	// the Outcome) or ctx is done (with failed{cancelled}).
	Hold <-chan struct{}

	mu sync.Mutex
	// started is signalled once per started job.
	started chan *runner.Job
}

// Started returns a channel that receives each job as its run starts (buffered; jobs beyond
// its capacity are not signalled).
func (s *stubRunner) Started() <-chan *runner.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started == nil {
		s.started = make(chan *runner.Job, 64)
	}
	return s.started
}

// Run implements runner.Runner.
func (s *stubRunner) Run(ctx context.Context, job *runner.Job, sink runner.Sink) (runner.Outcome, error) {
	s.mu.Lock()
	if s.started != nil {
		select {
		case s.started <- job:
		default:
		}
	}
	s.mu.Unlock()

	for _, p := range s.Progress {
		sink.Progress(p)
	}
	if s.Usage != nil {
		sink.Usage(*s.Usage)
	}
	if s.Hold != nil {
		select {
		case <-s.Hold:
		case <-ctx.Done():
			return runner.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonCancelled}, nil
		}
	}
	if s.Err != nil {
		return runner.Outcome{}, s.Err
	}
	out := s.Outcome
	if out.Status == "" {
		out.Status = runner.StatusDone
	}
	if out.Result != nil || out.Status != runner.StatusThrottled {
		var r v1alpha1.Result
		if out.Result != nil {
			r = *out.Result
		} else {
			r.Status = v1alpha1.ResultStatus(out.Status)
		}
		r.Task = job.TaskID()
		out.Result = &r
	}
	return out, nil
}
