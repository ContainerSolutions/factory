package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// Why a run's context ended (context.Cause).
var (
	errCancelRequested = errors.New("the dispatcher cancelled the task")
	errLeaseGone       = errors.New("the lease is gone")
	errShutdown        = errors.New("the agent is shutting down")
)

// maxPendingProgress bounds the progress events buffered between heartbeats (the Run keeps a
// ring of 200).
const maxPendingProgress = 200

// maxSummary bounds a failure summary built from a runner error.
const maxSummary = 500

// execute runs one leased task: heartbeats (the first at once, so the task turns Running),
// the runner, then the report. ctx is the worker's: when it ends, the run is cancelled and
// abandoned with shutdown.
func (w *Worker) execute(ctx context.Context, l *apiclient.Lease) {
	log := w.log.With("task", l.Task.Metadata.ID, "lease", l.Lease.Id)
	start := w.clk.Now()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	sink := &batch{clk: w.clk}
	job := &runner.Job{
		Lease: *l, Git: runner.NewCredentials(l.GitCredential, l.ReadCredentials...), API: w.api,
		Dispatcher: w.o.Dispatcher, AgentToken: w.Token, StartedAt: start,
	}
	log.Info("task leased", "taskSpec", l.Task.Spec.TaskSpec.Name, "queue", l.Task.Spec.Queue, "heartbeatEvery", l.Lease.HeartbeatEvery)

	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); w.heartbeats(ctx, l, sink, job.Git, cancel, stop, log) }()
	go func() { defer bg.Done(); w.drainDeadline(runCtx, start, l.Task, cancel, stop, log) }()

	out, err := w.o.Runner.Run(runCtx, job, sink)
	close(stop)
	bg.Wait()

	cause := context.Cause(runCtx)
	if ctx.Err() != nil {
		cause = errShutdown // aborted
	}
	rctx := context.WithoutCancel(ctx)
	switch {
	case errors.Is(cause, errLeaseGone):
		log.Warn("the lease is gone: the run was stopped and nothing is reported")
		return
	case errors.Is(cause, errCancelRequested):
		if w.flush(rctx, l, sink, job.Git, log) {
			// Abandoning a Cancelling task is the acknowledgement: it becomes Cancelled.
			w.abandon(rctx, l, apiclient.AbandonRequestReasonAgentError, nil, "cancelled by the dispatcher", log)
		}
		return
	case errors.Is(cause, errShutdown) && !finishedAnyway(out, err):
		if w.flush(rctx, l, sink, job.Git, log) {
			w.abandon(rctx, l, apiclient.AbandonRequestReasonShutdown, nil, "the agent shut down before the run finished", log)
		}
		return
	}
	if !w.flush(rctx, l, sink, job.Git, log) {
		return
	}
	w.report(rctx, l, out, err, log)
}

// finishedAnyway: the run produced its outcome before the shutdown took effect, so it is
// reported rather than thrown away.
func finishedAnyway(out runner.Outcome, err error) bool {
	if err != nil {
		return false
	}
	return out.Status == runner.StatusThrottled || (out.Result != nil && out.Reason != runner.ReasonCancelled)
}

// report sends a finished run's Outcome (supervise.md §Lease loop): throttled → abandon
// {throttled, until} and PATCH {Throttled, until}; an error or no Result → complete
// {status: failed}; otherwise complete {result, transcriptRef}.
func (w *Worker) report(ctx context.Context, l *apiclient.Lease, out runner.Outcome, runErr error, log *slog.Logger) {
	taskID := l.Task.Metadata.ID
	switch {
	case runErr == nil && out.Status == runner.StatusThrottled:
		until := w.clk.Now().Add(DefaultThrottle)
		if out.ThrottledUntil != nil && out.ThrottledUntil.After(w.clk.Now()) {
			until = *out.ThrottledUntil
		}
		until = until.UTC()
		w.setThrottled(until)
		log.Info("the harness is throttled: handing the task back", "until", until)
		w.abandon(ctx, l, apiclient.AbandonRequestReasonThrottled, &until, out.Reason, log)
		if err := w.update(ctx, "report throttled", apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentThrottled), Until: &until}); err != nil {
			log.Warn("reporting Throttled failed", "err", err)
		}
	case runErr != nil || out.Result == nil:
		reason := out.Reason
		if runErr != nil || reason == "" {
			reason = runner.ReasonHarnessError
		}
		summary := reason
		if runErr != nil {
			summary += ": " + runErr.Error()
			log.Debug("the run broke", "err", runErr)
		}
		if len(summary) > maxSummary {
			summary = summary[:maxSummary]
		}
		log.Warn("the run failed without a result", "reason", reason)
		w.complete(ctx, l, v1alpha1.Result{Task: taskID, Status: v1alpha1.ResultFailed, Summary: summary}, out.TranscriptRef, log)
	default:
		res := *out.Result
		if res.Task == "" {
			res.Task = taskID
		}
		w.complete(ctx, l, res, out.TranscriptRef, log)
	}
}

// complete posts the Result, retrying transient failures.
func (w *Worker) complete(ctx context.Context, l *apiclient.Lease, res v1alpha1.Result, transcriptRef string, log *slog.Logger) {
	body := apiclient.CompleteRequest{Result: res}
	if transcriptRef != "" {
		body.TranscriptRef = &transcriptRef
	}
	if w.send(ctx, "complete", log, func(ctx context.Context) error {
		res, err := w.api.CompleteLeaseWithResponse(ctx, l.Lease.Id, body)
		if err != nil {
			return err
		}
		if res.StatusCode() >= 300 {
			return apiError("complete", res.HTTPResponse, res.Body)
		}
		return nil
	}) {
		log.Info("task completed", "status", res.Status, "outcome", res.Outcome)
	}
}

// abandon hands the task back, retrying transient failures.
func (w *Worker) abandon(ctx context.Context, l *apiclient.Lease, reason apiclient.AbandonRequestReason, until *time.Time, message string, log *slog.Logger) {
	body := apiclient.AbandonRequest{Reason: reason, Until: until}
	if message != "" {
		body.Message = &message
	}
	if w.send(ctx, "abandon", log, func(ctx context.Context) error {
		res, err := w.api.AbandonLeaseWithResponse(ctx, l.Lease.Id, body)
		if err != nil {
			return err
		}
		if res.StatusCode() >= 300 {
			return apiError("abandon", res.HTTPResponse, res.Body)
		}
		return nil
	}) {
		log.Info("task abandoned", "reason", reason)
	}
}

// send runs a lease call with retries on transient failures; false when it gave up or the
// lease is gone.
func (w *Worker) send(ctx context.Context, op string, log *slog.Logger, call func(context.Context) error) bool {
	b := w.newBackoff()
	for attempt := 1; ; attempt++ {
		err := w.authed(ctx, func(ctx context.Context) error {
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			return call(cctx)
		})
		switch {
		case err == nil:
			return true
		case isGone(err):
			log.Warn(op+": the lease is already gone", "err", err)
			return false
		case !isTransient(err) || attempt >= w.o.ReportAttempts:
			log.Error(op+" failed", "err", err, "attempts", attempt)
			return false
		}
		d := b.next()
		log.Warn(op+" failed; retrying", "err", err, "in", d)
		if !w.sleep(ctx, d) {
			return false
		}
	}
}

// heartbeats beats at once and then every lease.heartbeatEvery until stop.
func (w *Worker) heartbeats(ctx context.Context, l *apiclient.Lease, sink *batch, git *runner.Credentials,
	cancel context.CancelCauseFunc, stop <-chan struct{}, log *slog.Logger) {
	every := DefaultHeartbeat
	if d, err := time.ParseDuration(l.Lease.HeartbeatEvery); err == nil && d >= time.Second {
		every = d
	}
	for {
		if !w.beat(ctx, l, sink, git, cancel, log) {
			return
		}
		t := w.clk.NewTimer(every)
		select {
		case <-stop:
			t.Stop()
			return
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}

// beat sends one heartbeat with the batched progress and usage. A lost lease cancels the run
// (false); a cancel request cancels it too but beats go on until the run stops; a failed
// heartbeat keeps the batch for the next one.
func (w *Worker) beat(ctx context.Context, l *apiclient.Lease, sink *batch, git *runner.Credentials,
	cancel context.CancelCauseFunc, log *slog.Logger) bool {
	req, taken := sink.take()
	var resp *apiclient.HeartbeatResponse
	err := w.authed(ctx, func(ctx context.Context) error {
		cctx, c := context.WithTimeout(ctx, callTimeout)
		defer c()
		res, err := w.api.HeartbeatLeaseWithResponse(cctx, l.Lease.Id, req)
		if err != nil {
			return err
		}
		if res.JSON200 == nil {
			return apiError("heartbeat", res.HTTPResponse, res.Body)
		}
		resp = res.JSON200
		return nil
	})
	if err != nil {
		if isGone(err) {
			log.Warn("heartbeat: the lease is gone; stopping the run", "err", err)
			cancel(errLeaseGone)
			return false
		}
		sink.restore(taken)
		if ctx.Err() == nil {
			log.Warn("heartbeat failed; retrying at the next beat", "err", err)
		}
		return true
	}
	if c := resp.RefreshedGitCredential; c != nil {
		git.Set(*c)
		log.Info("git credential refreshed", "expiresAt", c.ExpiresAt)
	}
	if rc := resp.RefreshedReadCredentials; len(rc) > 0 {
		git.SetReads(rc)
		log.Info("read-only git credentials refreshed", "count", len(rc))
	}
	if resp.Cancel {
		log.Info("the dispatcher cancelled the task; stopping the run")
		cancel(errCancelRequested)
	}
	return true
}

// flush sends what the run reported since the last heartbeat, before the final report; false
// when the lease is gone.
func (w *Worker) flush(ctx context.Context, l *apiclient.Lease, sink *batch, git *runner.Credentials, log *slog.Logger) bool {
	if sink.empty() {
		return true
	}
	gone := false
	w.beat(ctx, l, sink, git, func(cause error) { gone = errors.Is(cause, errLeaseGone) }, log)
	return !gone
}

// drainDeadline cancels the run (errShutdown) when the worker drains and the task's
// wall-clock budget (limits.maxWallClock from the run's start) is spent.
func (w *Worker) drainDeadline(runCtx context.Context, start time.Time, t v1alpha1.Task,
	cancel context.CancelCauseFunc, stop <-chan struct{}, log *slog.Logger) {
	select {
	case <-w.drainCh:
	case <-stop:
		return
	case <-runCtx.Done():
		return
	}
	budget := w.o.DefaultWallClock
	if lim := t.Spec.Limits; lim != nil && lim.MaxWallClock != "" {
		if d, err := lim.MaxWallClock.Parse(); err == nil && d > 0 {
			budget = d
		}
	}
	deadline := start.Add(budget)
	log.Info("draining: the run may finish until its wall-clock budget is spent", "deadline", deadline)
	timer := w.clk.NewTimerAt(deadline)
	defer timer.Stop()
	select {
	case <-timer.C():
		log.Warn("draining: the wall-clock budget is spent; stopping the run")
		cancel(errShutdown)
	case <-stop:
	case <-runCtx.Done():
	}
}

// batch is the runner's Sink: progress and usage deltas waiting for the next heartbeat.
type batch struct {
	clk      clock.Clock
	mu       sync.Mutex
	progress []v1alpha1.ProgressEvent
	usage    runner.UsageDelta
	hasUsage bool
	dropped  int
}

// taken is what one heartbeat carried, to put back when it failed.
type taken struct {
	progress []v1alpha1.ProgressEvent
	usage    runner.UsageDelta
	hasUsage bool
}

// Progress implements runner.Sink.
func (b *batch) Progress(e v1alpha1.ProgressEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.progress = append(b.progress, e)
	b.trimLocked()
}

// Usage implements runner.Sink.
func (b *batch) Usage(d runner.UsageDelta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	addUsage(&b.usage, d)
	b.hasUsage = true
}

func addUsage(sum *runner.UsageDelta, d runner.UsageDelta) {
	sum.InputTokens += d.InputTokens
	sum.OutputTokens += d.OutputTokens
	if d.CostUSD != nil {
		c := *d.CostUSD
		if sum.CostUSD != nil {
			c += *sum.CostUSD
		}
		sum.CostUSD = &c
	}
}

func (b *batch) trimLocked() {
	if n := len(b.progress) - maxPendingProgress; n > 0 {
		b.progress = append([]v1alpha1.ProgressEvent(nil), b.progress[n:]...)
		b.dropped += n
	}
}

func (b *batch) empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.progress) == 0 && !b.hasUsage
}

// take empties the batch into a heartbeat request.
func (b *batch) take() (apiclient.HeartbeatRequest, taken) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := taken{progress: b.progress, usage: b.usage, hasUsage: b.hasUsage}
	req := apiclient.HeartbeatRequest{Progress: b.progress}
	if b.dropped > 0 {
		req.Progress = append([]v1alpha1.ProgressEvent{{
			At: b.clk.Now().UTC(), Kind: "message",
			Message: fmt.Sprintf("%d progress events dropped (the dispatcher was unreachable)", b.dropped),
		}}, req.Progress...)
		b.dropped = 0
	}
	if b.hasUsage {
		u := &apiclient.UsageDelta{InputTokens: ptr(b.usage.InputTokens), OutputTokens: ptr(b.usage.OutputTokens)}
		if b.usage.CostUSD != nil {
			u.CostUSD = ptr(float32(*b.usage.CostUSD))
		}
		req.Usage = u
	}
	b.progress, b.usage, b.hasUsage = nil, runner.UsageDelta{}, false
	return req, t
}

// restore puts a failed heartbeat's batch back in front of what arrived since.
func (b *batch) restore(t taken) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.progress = append(t.progress, b.progress...)
	b.trimLocked()
	if t.hasUsage {
		addUsage(&b.usage, t.usage)
		b.hasUsage = true
	}
}
