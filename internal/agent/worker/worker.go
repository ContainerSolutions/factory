// Package worker is factory-agent's worker loop (supervise.md §The worker loop): it registers
// with the dispatcher (its refresh token, else the one-time enrollment token), long-polls for
// leases, runs each leased task through a runner.Runner while heartbeating, reports the
// Outcome (complete, abandon, throttle), and drains on SIGTERM.
//
// The agent token lives in memory only: a restart registers again with the refresh token, which
// rotates on each registration. Tokens are never logged.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// Defaults.
const (
	// DefaultLeaseWait is the long poll's wait (the dispatcher caps it at 60 s).
	DefaultLeaseWait = 30 * time.Second
	// DefaultHeartbeat is used when a lease doesn't say (pool.lease.heartbeat is 30 s).
	DefaultHeartbeat = 30 * time.Second
	// DefaultWallClock bounds a drain for a task whose TaskSpec sets no maxWallClock.
	DefaultWallClock = time.Hour
	// DefaultThrottle is how long a throttled harness rests when it gave no reset time
	// (harness-adapters.md: else +15 min).
	DefaultThrottle = 15 * time.Minute
	// DefaultMinBackoff and DefaultMaxBackoff bound the exponential backoff on failed calls.
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = time.Minute
	// DefaultReportAttempts is how often complete and abandon are tried on transient errors.
	DefaultReportAttempts = 5
)

// Enrollment is an enrolled agent's registration credential (P1 §6; enroll.Files).
type Enrollment interface {
	// Token is the refresh token when one is saved (refresh true), else the one-time
	// enrollment token.
	Token() (tok string, refresh bool, err error)
	// Save keeps the next refresh token durably; it is called before the registration that
	// returned it is used.
	Save(refresh string) error
	// Forget drops a refresh token the dispatcher refused.
	Forget() error
}

// Options configure a Worker. Pool, Host, AgentVersion, Capabilities, Enrollment and Runner
// are required.
type Options struct {
	// Dispatcher is the dispatcher's base URL (the API is under BasePath).
	Dispatcher   string
	Pool         string
	Host         string
	AgentVersion string
	Capabilities v1alpha1.AgentCapabilities
	Lending      v1alpha1.Lending

	// Enrollment registers with the refresh token (else the enrollment token) and saves each
	// rotated one.
	Enrollment Enrollment
	// Runner executes leased tasks.
	Runner runner.Runner

	// HTTPClient calls the dispatcher. Default: http.DefaultClient's transport, no timeout
	// (each call carries its own deadline).
	HTTPClient apiclient.HttpRequestDoer
	Clock      clock.Clock
	Logger     *slog.Logger

	// LeaseWait is the long poll's wait, whole seconds in [1s, 60s]. Default 30 s.
	LeaseWait time.Duration
	// DefaultWallClock bounds a drain for tasks without limits.maxWallClock. Default 1 h.
	DefaultWallClock time.Duration
	// MinBackoff and MaxBackoff bound retries. Defaults 1 s and 1 min.
	MinBackoff, MaxBackoff time.Duration
	// ReportAttempts bounds complete/abandon retries. Default 5.
	ReportAttempts int
}

// Worker is one registered agent's lease loop.
type Worker struct {
	o   Options
	api *apiclient.ClientWithResponses
	clk clock.Clock
	log *slog.Logger

	mu             sync.Mutex
	agentID        string
	token          string
	lending        v1alpha1.Lending
	throttledUntil time.Time
	fatal          error

	regMu sync.Mutex // serialises (re-)registration
	slots *slots

	drainOnce sync.Once
	drainCh   chan struct{}
}

// New checks the options and builds a Worker.
func New(o Options) (*Worker, error) {
	var missing []string
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"Dispatcher", o.Dispatcher != ""}, {"Pool", o.Pool != ""}, {"Host", o.Host != ""},
		{"AgentVersion", o.AgentVersion != ""}, {"Capabilities.Harnesses", len(o.Capabilities.Harnesses) > 0},
		{"Enrollment", o.Enrollment != nil}, {"Runner", o.Runner != nil},
	} {
		if !f.ok {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("worker: missing options %v", missing)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LeaseWait == 0 {
		o.LeaseWait = DefaultLeaseWait
	}
	if o.LeaseWait < time.Second || o.LeaseWait > 60*time.Second || o.LeaseWait%time.Second != 0 {
		return nil, fmt.Errorf("worker: LeaseWait %s must be whole seconds between 1s and 60s", o.LeaseWait)
	}
	if o.DefaultWallClock <= 0 {
		o.DefaultWallClock = DefaultWallClock
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(DefaultMaxBackoff, o.MinBackoff)
	}
	if o.ReportAttempts <= 0 {
		o.ReportAttempts = DefaultReportAttempts
	}
	w := &Worker{
		o: o, clk: o.Clock, log: o.Logger, lending: o.Lending,
		slots:   newSlots(maxConcurrent(o.Lending)),
		drainCh: make(chan struct{}),
	}
	api, err := apiclient.NewClientWithResponses(APIBase(o.Dispatcher),
		apiclient.WithHTTPClient(o.HTTPClient), apiclient.WithRequestEditorFn(w.authorize))
	if err != nil {
		return nil, fmt.Errorf("worker: dispatcher client: %w", err)
	}
	w.api = api
	return w, nil
}

func maxConcurrent(l v1alpha1.Lending) int {
	if l.MaxConcurrent > 0 {
		return l.MaxConcurrent
	}
	return 1
}

// authorize adds the agent token to every call but registration.
func (w *Worker) authorize(_ context.Context, r *http.Request) error {
	if tok := w.Token(); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// Token is the current agent token ("" before registration). Never log it.
func (w *Worker) Token() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.token
}

// AgentID is the registered agent's id ("" before registration).
func (w *Worker) AgentID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agentID
}

// Drain stops leasing: running tasks finish (or are abandoned with shutdown once their
// wall-clock budget is spent), then the agent deregisters and Run returns. Safe to call more
// than once and from any goroutine.
func (w *Worker) Drain() { w.drainOnce.Do(func() { close(w.drainCh) }) }

// fail records a fatal error (the first one) and drains.
func (w *Worker) fail(err error) {
	w.mu.Lock()
	if w.fatal == nil {
		w.fatal = err
	}
	w.mu.Unlock()
	w.Drain()
}

func (w *Worker) fatalErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fatal
}

// Throttled is when the agent may lease again after a throttled run (zero when it isn't
// throttled).
func (w *Worker) Throttled() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.throttledUntil
}

func (w *Worker) setThrottled(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if until.After(w.throttledUntil) {
		w.throttledUntil = until
	}
}

// SetLending changes the owner's consent (SIGHUP): the local concurrency now, and the
// dispatcher's copy through PATCH /agents/{id} once registered.
func (w *Worker) SetLending(ctx context.Context, l v1alpha1.Lending) error {
	w.mu.Lock()
	w.lending = l
	registered := w.agentID != ""
	w.mu.Unlock()
	w.slots.setCapacity(maxConcurrent(l))
	if !registered {
		return nil
	}
	if err := w.update(ctx, "update lending", apiclient.AgentUpdate{Lending: &l}); err != nil {
		return err
	}
	w.log.Info("lending updated", "maxConcurrent", maxConcurrent(l), "unlimited", l.Unlimited, "queues", l.Queues)
	return nil
}

// Run registers, then leases and runs tasks until Drain (or a fatal error), waits for the
// running tasks and deregisters. Cancelling ctx aborts: running tasks are cancelled and
// abandoned with shutdown. It returns nil after a drain or an abort, and the error when
// registration is refused.
func (w *Worker) Run(ctx context.Context) error {
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	go func() {
		select {
		case <-w.drainCh:
			stopLoop()
		case <-loopCtx.Done():
		}
	}()

	if err := w.register(loopCtx); err != nil {
		if loopCtx.Err() != nil && w.fatalErr() == nil {
			return nil // drained or aborted before registering
		}
		return fmt.Errorf("register: %w", err)
	}

	var running sync.WaitGroup
	b := w.newBackoff()
	for loopCtx.Err() == nil {
		if !w.slots.acquire(loopCtx) {
			break
		}
		// Checked with the slot in hand: a run that just ended throttled released it.
		if until := w.Throttled(); w.clk.Now().Before(until) {
			w.slots.release()
			w.log.Info("throttled: not leasing until the harness's limit resets", "until", until)
			w.sleepUntil(loopCtx, until)
			continue
		}
		l, err := w.lease(loopCtx)
		if err != nil {
			w.slots.release()
			if loopCtx.Err() != nil {
				break
			}
			d := b.next()
			w.log.Warn("lease failed; retrying", "err", err, "in", d)
			w.sleep(loopCtx, d)
			continue
		}
		b.reset()
		if l == nil {
			w.slots.release()
			continue
		}
		if loopCtx.Err() != nil {
			// Leased just as the drain began: hand it straight back.
			w.abandon(context.WithoutCancel(ctx), l, apiclient.AbandonRequestReasonShutdown, nil, "the agent is draining", w.log)
			w.slots.release()
			break
		}
		running.Add(1)
		go func() {
			defer running.Done()
			defer w.slots.release()
			w.execute(ctx, l)
		}()
	}

	if ctx.Err() == nil {
		w.log.Info("draining: no new leases; waiting for running tasks", "running", w.slots.inUse())
		if err := w.update(ctx, "report draining", apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentDraining)}); err != nil {
			w.log.Warn("reporting Draining failed", "err", err)
		}
	}
	running.Wait()
	w.deregister(context.WithoutCancel(ctx))
	return w.fatalErr()
}

func ptr[T any](v T) *T { return &v }

func (w *Worker) newBackoff() *backoff { return &backoff{min: w.o.MinBackoff, max: w.o.MaxBackoff} }

// sleep waits d on the clock; false when ctx ended first.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	t := w.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *Worker) sleepUntil(ctx context.Context, at time.Time) bool {
	t := w.clk.NewTimerAt(at)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// register registers (or registers again) with the enrollment credential, retrying transient
// failures with backoff.
func (w *Worker) register(ctx context.Context) error { return w.registerEnrolled(ctx) }

// registerEnrolled registers with the enrollment credential. The response's refresh token is
// saved before the new agent token is used: the presented one is spent. A refused refresh
// token is forgotten, so the next start reads enrollmentTokenFile.
func (w *Worker) registerEnrolled(ctx context.Context) error {
	tok, refresh, err := w.o.Enrollment.Token()
	if err != nil {
		return &permanentError{err}
	}
	header := HeaderEnrollmentToken
	if refresh {
		header = HeaderAgentRefreshToken
	}
	w.mu.Lock()
	lending := w.lending
	w.mu.Unlock()
	reg, err := w.post(ctx, apiclient.RegisterAgentRequest{
		Pool: w.o.Pool, Host: w.o.Host, AgentVersion: w.o.AgentVersion,
		Capabilities: w.o.Capabilities, Lending: &lending,
	}, map[string]string{header: tok})
	switch {
	case err != nil && refresh && isUnauthorized(err):
		if ferr := w.o.Enrollment.Forget(); ferr != nil {
			w.log.Error("the refused refresh token", "err", ferr)
		}
		return &permanentError{fmt.Errorf("the dispatcher refused the agent's refresh token (revoked, or spent by an interrupted registration): "+
			"mint a new enrollment with `factory agent enroll`, put its token in enrollmentTokenFile and restart: %w", err)}
	case err != nil && !refresh && (isUnauthorized(err) || statusOf(err) == http.StatusBadRequest):
		return &permanentError{fmt.Errorf("the dispatcher refused the enrollment token (spent, expired, revoked, or for another pool or host): "+
			"mint a new one with `factory agent enroll` for this pool and host, put it in enrollmentTokenFile and restart: %w", err)}
	case err != nil:
		return err
	case reg.RefreshToken == nil || *reg.RefreshToken == "":
		return &permanentError{errors.New("register: the dispatcher returned no refresh token")}
	}
	if err := w.o.Enrollment.Save(*reg.RefreshToken); err != nil {
		return &permanentError{fmt.Errorf("the next refresh token was not saved, and the one used is spent: "+
			"mint a new enrollment with `factory agent enroll`: %w", err)}
	}
	w.registered(reg)
	return nil
}

// post sends registerAgent with headers, retrying transient failures with backoff.
func (w *Worker) post(ctx context.Context, body apiclient.RegisterAgentRequest, headers map[string]string) (*apiclient.AgentRegistration, error) {
	creds := func(_ context.Context, r *http.Request) error {
		r.Header.Del("Authorization")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return nil
	}
	b := w.newBackoff()
	for {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		res, err := w.api.RegisterAgentWithResponse(cctx, body, creds)
		cancel()
		if err == nil && res.JSON201 != nil {
			return res.JSON201, nil
		}
		if err == nil {
			err = apiError("register", res.HTTPResponse, res.Body)
		}
		if !isTransient(err) {
			return nil, err
		}
		d := b.next()
		w.log.Warn("registration failed; retrying", "err", err, "in", d)
		if !w.sleep(ctx, d) {
			return nil, ctx.Err()
		}
	}
}

// registered keeps a registration's agent id and token.
func (w *Worker) registered(reg *apiclient.AgentRegistration) {
	w.mu.Lock()
	w.agentID, w.token = reg.AgentId, reg.AgentToken
	w.mu.Unlock()
	w.log.Info("registered", "agent", reg.AgentId, "pool", w.o.Pool,
		"harnesses", harnessNames(w.o.Capabilities), "labels", w.o.Capabilities.Labels, "tokenExpiresAt", reg.ExpiresAt)
}

func harnessNames(c v1alpha1.AgentCapabilities) []string {
	out := make([]string, 0, len(c.Harnesses))
	for _, h := range c.Harnesses {
		out = append(out, h.Name)
	}
	return slices.Sorted(slices.Values(out))
}

// reregister registers again after the token stale was refused, unless another call already
// did. A refused registration is fatal: the worker drains.
func (w *Worker) reregister(ctx context.Context, stale string) error {
	w.regMu.Lock()
	defer w.regMu.Unlock()
	if w.Token() != stale {
		return nil
	}
	w.log.Warn("the agent token was refused; registering again")
	if err := w.register(ctx); err != nil {
		if ctx.Err() == nil {
			w.fail(fmt.Errorf("register again: %w", err))
		}
		return err
	}
	return nil
}

// authed runs an agent call; when the token is refused (401) it registers again once and
// retries.
func (w *Worker) authed(ctx context.Context, call func(context.Context) error) error {
	tok := w.Token()
	err := call(ctx)
	if !isUnauthorized(err) || w.fatalErr() != nil {
		return err // after a refused registration, later calls don't register again
	}
	if rerr := w.reregister(ctx, tok); rerr != nil {
		return errors.Join(err, rerr)
	}
	return call(ctx)
}

// lease long-polls for a task: nil, nil when none came in time (204).
func (w *Worker) lease(ctx context.Context) (*apiclient.Lease, error) {
	wait := fmt.Sprintf("%ds", int(w.o.LeaseWait/time.Second))
	var out *apiclient.Lease
	err := w.authed(ctx, func(ctx context.Context) error {
		cctx, cancel := context.WithTimeout(ctx, w.o.LeaseWait+callTimeout)
		defer cancel()
		res, err := w.api.LeaseTaskWithResponse(cctx, w.AgentID(), &apiclient.LeaseTaskParams{Wait: &wait})
		if err != nil {
			return err
		}
		switch res.StatusCode() {
		case http.StatusOK:
			if res.JSON200 == nil {
				return fmt.Errorf("lease: undecodable 200 answer")
			}
			out = res.JSON200
		case http.StatusNoContent:
			out = nil
		default:
			return apiError("lease", res.HTTPResponse, res.Body)
		}
		return nil
	})
	return out, err
}

// update PATCHes the agent.
func (w *Worker) update(ctx context.Context, op string, u apiclient.AgentUpdate) error {
	return w.authed(ctx, func(ctx context.Context) error {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		res, err := w.api.UpdateAgentWithResponse(cctx, w.AgentID(), u)
		if err != nil {
			return err
		}
		if res.JSON200 == nil {
			return apiError(op, res.HTTPResponse, res.Body)
		}
		return nil
	})
}

// deregister revokes the agent token and marks the agent Offline: the token lives only in
// this process, so it can never be used again once Run returns.
func (w *Worker) deregister(ctx context.Context) {
	id := w.AgentID()
	if id == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := w.api.DeregisterAgentWithResponse(cctx, id)
	if err == nil && res.StatusCode() != http.StatusNoContent {
		err = apiError("deregister", res.HTTPResponse, res.Body)
	}
	w.mu.Lock()
	w.token = ""
	w.mu.Unlock()
	if err != nil {
		w.log.Warn("deregistering failed; the agent goes Offline when its token expires", "agent", id, "err", err)
		return
	}
	w.log.Info("deregistered", "agent", id)
}

// slots bounds the tasks run at once; its capacity follows lending.maxConcurrent.
type slots struct {
	mu       sync.Mutex
	capacity int
	used     int
	changed  chan struct{}
}

func newSlots(n int) *slots { return &slots{capacity: n, changed: make(chan struct{})} }

// acquire takes a slot, waiting for one; false when ctx ended first.
func (s *slots) acquire(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.used < s.capacity {
			s.used++
			s.mu.Unlock()
			return true
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func (s *slots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used--
	s.signalLocked()
}

func (s *slots) setCapacity(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capacity = n
	s.signalLocked()
}

func (s *slots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

func (s *slots) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
