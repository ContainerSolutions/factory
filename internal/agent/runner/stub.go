package runner

import (
	"context"
	"sync"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Stub is a Runner that reports canned progress and usage, optionally waits, and returns a
// canned Outcome, for worker tests and for development before the run pipeline exists. It is
// safe for concurrent use.
type Stub struct {
	// Outcome is returned for every job; its Result (if any) is copied with Task set to the
	// job's task. The zero Outcome is done with an empty Result.
	Outcome Outcome
	// Err, when set, is returned instead of the Outcome.
	Err error
	// Progress and Usage are reported to the sink before the run ends.
	Progress []v1alpha1.ProgressEvent
	Usage    *UsageDelta
	// Hold, when set, is waited on before returning: the run ends when Hold is closed (with
	// the Outcome) or ctx is done (with failed{cancelled}).
	Hold <-chan struct{}

	mu   sync.Mutex
	jobs []*Job
	// started is signalled once per started job.
	started chan *Job
}

// Started returns a channel that receives each job as its run starts (buffered; jobs beyond
// its capacity are not signalled, only recorded in Jobs).
func (s *Stub) Started() <-chan *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started == nil {
		s.started = make(chan *Job, 64)
	}
	return s.started
}

// Jobs returns the jobs run so far.
func (s *Stub) Jobs() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Job(nil), s.jobs...)
}

// Run implements Runner.
func (s *Stub) Run(ctx context.Context, job *Job, sink Sink) (Outcome, error) {
	s.mu.Lock()
	s.jobs = append(s.jobs, job)
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
			return Outcome{Status: StatusFailed, Reason: ReasonCancelled}, nil
		}
	}
	if s.Err != nil {
		return Outcome{}, s.Err
	}
	out := s.Outcome
	if out.Status == "" {
		out.Status = StatusDone
	}
	if out.Result != nil || out.Status != StatusThrottled {
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
