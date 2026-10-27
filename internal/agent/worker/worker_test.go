package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/owner"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/agent/worker"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// The whole protocol: register (credentials in the headers, once) → lease → heartbeat at
// once → the run's progress and usage flushed → complete with the Result → drain (PATCH
// Draining) → deregister. No secret reaches the logs.
func TestWorkerLifecycle(t *testing.T) {
	f := newFakeDispatcher(t)
	cost := 0.25
	stub := &stubRunner{
		Outcome:  runner.Outcome{Result: &v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "triaged", Summary: "labelled"}, TranscriptRef: "tr_1"},
		Progress: []v1alpha1.ProgressEvent{{At: t0, Kind: "turn", Message: "reading the issue"}},
		Usage:    &runner.UsageDelta{InputTokens: 100, OutputTokens: 20, CostUSD: &cost},
	}
	started := stub.Started()
	h := startWorker(t, f, stub)
	f.offer(testLease("1", nil))
	job := <-started
	f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	if err := h.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var seq []string
	for _, c := range f.Calls() {
		seq = append(seq, c.String())
	}
	// One heartbeat at once, plus the flush when the run reported after it.
	want := []string{"POST /agents", "GET /agents/agt_gus-e2e/lease", "POST /leases/1/heartbeat"}
	if len(seq) < 4 || !slices.Equal(seq[:3], want) {
		t.Fatalf("calls %v\nwant prefix %v", seq, want)
	}
	rest := slices.DeleteFunc(slices.Clone(seq[3:]), func(s string) bool { return s == "POST /leases/1/heartbeat" })
	if len(rest) == 0 || rest[0] != "POST /leases/1/complete" {
		t.Fatalf("calls %v: no complete after the heartbeats", seq)
	}
	if !slices.Contains(seq, "PATCH /agents/agt_gus-e2e") || seq[len(seq)-1] != "DELETE /agents/agt_gus-e2e" {
		t.Errorf("no PATCH Draining or the last call isn't the deregistration: %v", seq)
	}

	reg := f.find(http.MethodPost, "/agents")[0]
	if reg.Join != joinSecret || reg.Owner != ownerSecret || reg.Auth != "" {
		t.Errorf("registration headers: join %q owner %q auth %q", reg.Join, reg.Owner, reg.Auth)
	}
	body := decode[apiclient.RegisterAgentRequest](t, reg.Body)
	if body.Pool != "local" || body.Host != "e2e" || body.Lending == nil || body.Lending.Queues[0] != "poc" || len(body.Capabilities.Harnesses) != 1 {
		t.Errorf("registration body %+v", body)
	}
	if strings.Contains(string(reg.Body), ownerSecret) || strings.Contains(string(reg.Body), joinSecret) {
		t.Error("a credential is in the registration body")
	}
	for _, c := range f.Calls()[1:] {
		if c.Join != "" || c.Owner != "" || !strings.HasPrefix(c.Auth, "Bearer "+agentID+".agent-secret-") {
			t.Errorf("%s: join %q owner %q auth %q", c, c.Join, c.Owner, c.Auth)
		}
	}

	beats := f.find(http.MethodPost, "/heartbeat")
	var events []v1alpha1.ProgressEvent
	var in int64
	for _, b := range beats {
		hb := decode[apiclient.HeartbeatRequest](t, b.Body)
		events = append(events, hb.Progress...)
		if hb.Usage != nil {
			in += *hb.Usage.InputTokens
			if hb.Usage.CostUSD == nil || *hb.Usage.CostUSD != 0.25 {
				t.Errorf("usage cost %v", hb.Usage.CostUSD)
			}
		}
	}
	if len(events) != 1 || events[0].Message != "reading the issue" || in != 100 {
		t.Errorf("heartbeats carried %+v and %d input tokens", events, in)
	}

	done := decode[apiclient.CompleteRequest](t, f.find(http.MethodPost, "/complete")[0].Body)
	if done.Result.Task != "tsk_1" || done.Result.Outcome != "triaged" || done.TranscriptRef == nil || *done.TranscriptRef != "tr_1" {
		t.Errorf("complete %+v", done)
	}
	patch := decode[apiclient.AgentUpdate](t, f.find(http.MethodPatch, "/agents/"+agentID)[0].Body)
	if patch.Phase == nil || *patch.Phase != v1alpha1.AgentDraining {
		t.Errorf("drain PATCH %+v", patch)
	}

	if job.TaskID() != "tsk_1" || job.Git.Get().Token != gitSecret || job.AgentToken() != "" || job.Dispatcher != f.URL() {
		t.Errorf("job: task %s, dispatcher %s, token cleared after deregistration: %v", job.TaskID(), job.Dispatcher, job.AgentToken() == "")
	}
	if *h.owners != 1 {
		t.Errorf("the owner token was asked for %d times, want 1", *h.owners)
	}
	h.assertNoSecretsInLogs(t)
}

// Heartbeats go every lease.heartbeatEvery on the clock; a cancel answer cancels the run and
// the task is handed back (agent-error), which acknowledges the cancellation.
func TestHeartbeatCancel(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	f.setHeartbeat(func(n int) (int, any) {
		return http.StatusOK, apiclient.HeartbeatResponse{ExpiresAt: t0.Add(2 * time.Minute), Cancel: n == 3}
	})
	hold := make(chan struct{})
	stub := &stubRunner{Hold: hold}
	started := stub.Started()
	startWorker(t, f, stub, withClock(clk))
	f.offer(testLease("1", nil))
	<-started
	f.waitFor("the first heartbeat", func() bool { return len(f.find(http.MethodPost, "/heartbeat")) == 1 })

	for i := 2; i <= 3; i++ {
		clk.BlockUntil(1) // the heartbeat timer
		clk.Advance(30 * time.Second)
		f.waitFor("another heartbeat", func() bool { return len(f.find(http.MethodPost, "/heartbeat")) == i })
	}
	f.waitFor("abandon", func() bool { return len(f.find(http.MethodPost, "/abandon")) == 1 })
	ab := decode[apiclient.AbandonRequest](t, f.find(http.MethodPost, "/abandon")[0].Body)
	if ab.Reason != apiclient.AbandonRequestReasonAgentError || ab.Message == nil || !strings.Contains(*ab.Message, "cancelled") {
		t.Errorf("abandon %+v", ab)
	}
	if n := len(f.find(http.MethodPost, "/complete")); n != 0 {
		t.Errorf("%d completes after a cancel", n)
	}
	close(hold)
}

// A heartbeat answered 409 (lease gone) stops the run; nothing is reported for it.
func TestHeartbeatLeaseGone(t *testing.T) {
	f := newFakeDispatcher(t)
	f.fail("POST /heartbeat", http.StatusConflict)
	ran := make(chan error, 1)
	r := runner.Func(func(ctx context.Context, job *runner.Job, sink runner.Sink) (runner.Outcome, error) {
		<-ctx.Done()
		ran <- context.Cause(ctx)
		return runner.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonCancelled}, nil
	})
	h := startWorker(t, f, r)
	f.offer(testLease("1", nil))
	select {
	case err := <-ran:
		if err == nil || !strings.Contains(err.Error(), "lease is gone") {
			t.Errorf("run cancelled with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run wasn't cancelled")
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if n := len(f.find(http.MethodPost, "/complete")) + len(f.find(http.MethodPost, "/abandon")); n != 0 {
		t.Errorf("%d reports for a lost lease", n)
	}
}

// A heartbeat with refreshed git credentials (the task token and the read-only ones) swaps
// them in the job; a failed heartbeat keeps
// its batch for the next one.
func TestHeartbeatRefreshAndRetry(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	refreshed := apiclient.GitCredential{Token: "ghs_refreshed-secret", Repo: "compliance-framework/docs", ExpiresAt: t0.Add(2 * time.Hour)}
	refreshedRead := apiclient.GitCredential{Token: "ghs_refreshed-read-secret", Repo: "compliance-framework/private", Permissions: map[string]string{"contents": "read"}}
	f.setHeartbeat(func(n int) (int, any) {
		r := apiclient.HeartbeatResponse{ExpiresAt: t0.Add(2 * time.Minute)}
		if n == 1 { // the first answered beat (the scripted 503 isn't counted)
			r.RefreshedGitCredential = &refreshed
			r.RefreshedReadCredentials = []apiclient.GitCredential{refreshedRead}
		}
		return http.StatusOK, r
	})
	f.fail("POST /heartbeat", http.StatusServiceUnavailable)
	hold := make(chan struct{})
	var job *runner.Job
	gotJob := make(chan struct{})
	r := runner.Func(func(ctx context.Context, j *runner.Job, sink runner.Sink) (runner.Outcome, error) {
		job = j
		sink.Progress(v1alpha1.ProgressEvent{At: t0, Kind: "turn", Message: "kept"})
		close(gotJob)
		<-hold
		return runner.Outcome{Status: runner.StatusDone, Result: &v1alpha1.Result{Status: v1alpha1.ResultDone}}, nil
	})
	h := startWorker(t, f, r, withClock(clk))
	f.offer(testLease("1", nil))
	<-gotJob
	f.waitFor("the failed first heartbeat", func() bool { return len(f.find(http.MethodPost, "/heartbeat")) == 1 })
	clk.BlockUntil(1)
	clk.Advance(30 * time.Second)
	f.waitFor("the second heartbeat", func() bool { return len(f.find(http.MethodPost, "/heartbeat")) == 2 })
	clk.BlockUntil(1) // the refresh is applied before the next timer is armed
	if job.Git.Get().Token != "ghs_refreshed-secret" {
		t.Errorf("the credential wasn't refreshed: %v", job.Git)
	}
	if rc := job.Git.Reads(); len(rc) != 1 || rc[0].Token != "ghs_refreshed-read-secret" {
		t.Errorf("the read-only credentials weren't refreshed: %v", job.Git)
	}
	if s := job.Git.String(); strings.Contains(s, "secret") {
		t.Errorf("Credentials.String leaks the token: %s", s)
	}
	if hb := decode[apiclient.HeartbeatRequest](t, f.find(http.MethodPost, "/heartbeat")[1].Body); len(hb.Progress) != 1 || hb.Progress[0].Message != "kept" {
		t.Errorf("the second heartbeat didn't carry the failed one's batch: %+v", hb)
	}
	close(hold)
	f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	h.assertNoSecretsInLogs(t, "ghs_refreshed-secret", "ghs_refreshed-read-secret")
}

// A throttled run is handed back (abandon throttled{until}), the agent reports Throttled
// until then, and it doesn't lease again before the clock gets there.
func TestThrottle(t *testing.T) {
	tests := []struct {
		name  string
		until *time.Time
		want  time.Time
	}{
		{"reset time from the harness", ptr(t0.Add(time.Hour)), t0.Add(time.Hour)},
		{"no reset time: +15 min", nil, t0.Add(worker.DefaultThrottle)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDispatcher(t)
			clk := clock.NewFake(t0)
			stub := &stubRunner{Outcome: runner.Outcome{Status: runner.StatusThrottled, Reason: "usage limit", ThrottledUntil: tt.until}}
			h := startWorker(t, f, stub, withClock(clk))
			f.offer(testLease("1", nil))
			f.waitFor("the Throttled PATCH", func() bool { return len(f.find(http.MethodPatch, "/agents/"+agentID)) == 1 })

			ab := decode[apiclient.AbandonRequest](t, f.find(http.MethodPost, "/abandon")[0].Body)
			if ab.Reason != apiclient.AbandonRequestReasonThrottled || ab.Until == nil || !ab.Until.Equal(tt.want) {
				t.Errorf("abandon %+v, want throttled until %s", ab, tt.want)
			}
			patch := decode[apiclient.AgentUpdate](t, f.find(http.MethodPatch, "/agents/"+agentID)[0].Body)
			if patch.Phase == nil || *patch.Phase != v1alpha1.AgentThrottled || patch.Until == nil || !patch.Until.Equal(tt.want) {
				t.Errorf("PATCH %+v", patch)
			}
			if !h.w.Throttled().Equal(tt.want) {
				t.Errorf("Throttled() = %s", h.w.Throttled())
			}
			if n := len(f.find(http.MethodPost, "/complete")); n != 0 {
				t.Errorf("%d completes for a throttled run", n)
			}

			// No lease poll while throttled, however long we look.
			polls := len(f.find(http.MethodGet, "/lease"))
			clk.BlockUntil(1) // the throttle timer
			clk.Advance(tt.want.Sub(t0) - time.Second)
			time.Sleep(50 * time.Millisecond)
			if n := len(f.find(http.MethodGet, "/lease")); n != polls {
				t.Fatalf("leased while throttled (%d polls, had %d)", n, polls)
			}
			clk.Advance(time.Second)
			f.waitFor("a poll after the throttle", func() bool { return len(f.find(http.MethodGet, "/lease")) == polls+1 })
			if err := h.stop(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Drain: PATCH Draining, no more polls, the running task finishes and completes, then the
// agent deregisters.
func TestDrainWaitsForTheRun(t *testing.T) {
	f := newFakeDispatcher(t)
	hold := make(chan struct{})
	stub := &stubRunner{Hold: hold, Outcome: runner.Outcome{Result: &v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "triaged"}}}
	started := stub.Started()
	h := startWorker(t, f, stub)
	f.offer(testLease("1", nil))
	<-started
	h.w.Drain()
	f.waitFor("PATCH Draining", func() bool { return len(f.find(http.MethodPatch, "/agents/"+agentID)) == 1 })
	polls := len(f.find(http.MethodGet, "/lease"))
	select {
	case err := <-h.done:
		t.Fatalf("Run returned with a task running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(hold)
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if n := len(f.find(http.MethodPost, "/complete")); n != 1 {
		t.Errorf("%d completes", n)
	}
	if n := len(f.find(http.MethodGet, "/lease")); n != polls {
		t.Errorf("polled after the drain (%d → %d)", polls, n)
	}
	calls := f.Calls()
	if last := calls[len(calls)-1]; last.String() != "DELETE /agents/"+agentID {
		t.Errorf("last call %s, want the deregistration", last)
	}
}

// Drain with a run that outlives its wall-clock budget: once limits.maxWallClock is spent
// (on the clock, from the run's start) the run is cancelled and abandoned with shutdown.
func TestDrainDeadline(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	stub := &stubRunner{Hold: make(chan struct{})}
	started := stub.Started()
	h := startWorker(t, f, stub, withClock(clk))
	f.offer(testLease("1", &v1alpha1.Limits{MaxWallClock: "10m"}))
	<-started
	f.waitFor("the first heartbeat", func() bool { return len(f.find(http.MethodPost, "/heartbeat")) == 1 })
	h.w.Drain()
	clk.BlockUntil(2) // the heartbeat timer and the drain deadline
	clk.Advance(9 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := len(f.find(http.MethodPost, "/abandon")); n != 0 {
		t.Fatalf("abandoned before the deadline")
	}
	clk.Advance(time.Minute)
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	abandons := f.find(http.MethodPost, "/abandon")
	if len(abandons) != 1 {
		t.Fatalf("%d abandons", len(abandons))
	}
	if ab := decode[apiclient.AbandonRequest](t, abandons[0].Body); ab.Reason != apiclient.AbandonRequestReasonShutdown {
		t.Errorf("abandon %+v", ab)
	}
}

// Cancelling Run's context (the second signal) abandons the running task with shutdown at
// once and deregisters.
func TestAbort(t *testing.T) {
	f := newFakeDispatcher(t)
	stub := &stubRunner{Hold: make(chan struct{})}
	started := stub.Started()
	h := startWorker(t, f, stub)
	f.offer(testLease("1", nil))
	<-started
	h.w.Drain()
	f.waitFor("PATCH Draining", func() bool { return len(f.find(http.MethodPatch, "/agents/"+agentID)) == 1 })
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return after the abort")
	}
	abandons := f.find(http.MethodPost, "/abandon")
	if len(abandons) != 1 {
		t.Fatalf("%d abandons", len(abandons))
	}
	if ab := decode[apiclient.AbandonRequest](t, abandons[0].Body); ab.Reason != apiclient.AbandonRequestReasonShutdown {
		t.Errorf("abandon %+v", ab)
	}
	if n := len(f.find(http.MethodDelete, "/agents/"+agentID)); n != 1 {
		t.Errorf("%d deregistrations", n)
	}
}

// Failed lease polls back off exponentially on the clock (1 s, 2 s, …) and recover.
func TestLeaseBackoff(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	f.fail("GET /lease", http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusInternalServerError)
	h := startWorker(t, f, &stubRunner{}, withClock(clk))
	for i, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		f.waitFor("a failed poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == i+1 })
		clk.BlockUntil(1)
		clk.Advance(wait - time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		if n := len(f.find(http.MethodGet, "/lease")); n != i+1 {
			t.Fatalf("polled again before %s", wait)
		}
		clk.Advance(time.Millisecond)
	}
	f.waitFor("a good poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == 4 })
	f.offer(testLease("1", nil))
	f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// A refused agent token (401) makes the worker register again (a new owner login) and carry
// on.
func TestReregisterOnUnauthorized(t *testing.T) {
	f := newFakeDispatcher(t)
	h := startWorker(t, f, &stubRunner{})
	f.waitFor("the first poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == 1 })
	f.revoke()
	f.offer(testLease("1", nil)) // the parked poll still answers with the old token's request…
	f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	f.offer(testLease("2", nil))
	f.waitFor("a second registration", func() bool { return len(f.find(http.MethodPost, "/agents")) == 2 })
	f.waitFor("the second complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 2 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if *h.owners != 2 {
		t.Errorf("owner logins %d, want 2", *h.owners)
	}
}

// A refused registration is permanent: Run returns the error without retrying.
func TestRegisterRefused(t *testing.T) {
	f := newFakeDispatcher(t)
	w, err := worker.New(worker.Options{
		Dispatcher: f.URL(), Pool: "local", Host: "e2e", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}},
		JoinToken:    func() (string, error) { return "wrong", nil },
		Owner:        owner.TokenFunc(func(context.Context) (string, error) { return ownerSecret, nil }),
		Runner:       &stubRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = w.Run(context.Background())
	var ae *worker.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("Run = %v, want a 401 without the token", err)
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}

	// No join token, or the owner refused the login: refused before any call.
	for name, o := range map[string]worker.Options{
		"join": {JoinToken: func() (string, error) { return "", errors.New("join-token: mode 0644") }, Owner: owner.TokenFunc(func(context.Context) (string, error) { return ownerSecret, nil })},
		"owner": {JoinToken: func() (string, error) { return joinSecret, nil }, Owner: owner.TokenFunc(func(context.Context) (string, error) {
			return "", owner.ErrDenied
		})},
	} {
		o.Dispatcher, o.Pool, o.Host, o.AgentVersion, o.Runner = f.URL(), "local", "e2e", "test", &stubRunner{}
		o.Capabilities = v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}}
		w, err := worker.New(o)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Run(context.Background()); err == nil {
			t.Errorf("%s: Run = nil", name)
		}
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("%d calls after the local refusals, want 1", n)
	}
}

// Registration retries transient failures (the dispatcher is restarting).
func TestRegisterRetries(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	f.fail("POST /agents", http.StatusServiceUnavailable)
	h := startWorker(t, f, &stubRunner{}, withClock(clk))
	f.waitFor("the failed registration", func() bool { return len(f.find(http.MethodPost, "/agents")) == 1 })
	clk.BlockUntil(1)
	clk.Advance(time.Second)
	f.waitFor("the registration", func() bool { return len(f.find(http.MethodGet, "/lease")) == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if *h.owners != 1 {
		t.Errorf("owner logins %d, want 1 (the token is reused across retries)", *h.owners)
	}
}

// A runner error or an outcome without a Result completes the task as failed.
func TestRunFailures(t *testing.T) {
	tests := []struct {
		name string
		r    runner.Runner
		want string
	}{
		{"runner error", &stubRunner{Err: errors.New("clone failed")}, "harness-error: clone failed"},
		{"no result", runner.Func(func(context.Context, *runner.Job, runner.Sink) (runner.Outcome, error) {
			return runner.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonInvalidResult}, nil
		}), "invalid-result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDispatcher(t)
			h := startWorker(t, f, tt.r)
			f.offer(testLease("1", nil))
			f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
			if err := h.stop(t); err != nil {
				t.Fatal(err)
			}
			done := decode[apiclient.CompleteRequest](t, f.find(http.MethodPost, "/complete")[0].Body)
			if done.Result.Status != v1alpha1.ResultFailed || done.Result.Task != "tsk_1" || done.Result.Summary != tt.want {
				t.Errorf("complete %+v", done.Result)
			}
		})
	}
}

// Complete retries transient failures and gives up on a gone lease.
func TestCompleteRetries(t *testing.T) {
	f := newFakeDispatcher(t)
	clk := clock.NewFake(t0)
	f.fail("POST /complete", http.StatusBadGateway)
	h := startWorker(t, f, &stubRunner{}, withClock(clk))
	f.offer(testLease("1", nil))
	f.waitFor("the failed complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	clk.BlockUntil(1) // the backoff (the heartbeats ended with the run; the loop waits for the slot)
	clk.Advance(time.Second)
	f.waitFor("the retried complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 2 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// Concurrency follows lending.maxConcurrent, and SetLending changes it and PATCHes lending.
func TestConcurrencyAndLending(t *testing.T) {
	f := newFakeDispatcher(t)
	hold := make(chan struct{})
	stub := &stubRunner{Hold: hold}
	started := stub.Started()
	h := startWorker(t, f, stub, withLending(v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 2}))
	f.offer(testLease("1", nil))
	f.offer(testLease("2", nil))
	<-started
	<-started
	time.Sleep(50 * time.Millisecond)
	polls := len(f.find(http.MethodGet, "/lease"))
	if polls != 2 {
		t.Errorf("%d polls with both slots busy, want 2", polls)
	}
	if err := h.w.SetLending(context.Background(), v1alpha1.Lending{Queues: []string{"poc", "validation"}, MaxConcurrent: 3}); err != nil {
		t.Fatal(err)
	}
	f.waitFor("a third poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == 3 })
	patch := decode[apiclient.AgentUpdate](t, f.find(http.MethodPatch, "/agents/"+agentID)[0].Body)
	if patch.Lending == nil || patch.Lending.MaxConcurrent != 3 || len(patch.Lending.Queues) != 2 || patch.Phase != nil {
		t.Errorf("lending PATCH %+v", patch)
	}
	close(hold)
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := worker.New(worker.Options{}); err == nil || !strings.Contains(err.Error(), "Runner") {
		t.Errorf("New({}) = %v", err)
	}
	o := worker.Options{
		Dispatcher: "http://127.0.0.1:1", Pool: "p", Host: "h", AgentVersion: "v",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}},
		JoinToken:    func() (string, error) { return "", nil }, Owner: owner.TokenFunc(nil), Runner: &stubRunner{},
		LeaseWait: 90 * time.Second,
	}
	if _, err := worker.New(o); err == nil || !strings.Contains(err.Error(), "LeaseWait") {
		t.Errorf("New(LeaseWait 90s) = %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// When registering again is refused (the owner left the pool's owners), the worker stops
// with that error and doesn't keep registering.
func TestReregisterRefused(t *testing.T) {
	f := newFakeDispatcher(t)
	h := startWorker(t, f, &stubRunner{})
	f.waitFor("the first poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == 1 })
	f.revoke()
	f.fail("POST /agents", http.StatusForbidden)
	f.offer(testLease("1", nil)) // answers the parked poll; the heartbeat then gets a 401
	select {
	case err := <-h.done:
		h.done <- err
		var ae *worker.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
			t.Fatalf("Run = %v, want the refused registration", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't stop")
	}
	if n := len(f.find(http.MethodPost, "/agents")); n != 2 || *h.owners != 2 {
		t.Errorf("%d registrations and %d owner logins, want 2 each", n, *h.owners)
	}
}
