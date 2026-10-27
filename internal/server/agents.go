package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
)

var waitRE = regexp.MustCompile(`^[0-9]{1,2}s$`)

// RegisterAgent implements gen.StrictServerInterface.
func (s *Server) RegisterAgent(ctx context.Context, req gen.RegisterAgentRequestObject) (gen.RegisterAgentResponseObject, error) {
	if s.d.Catalog.Current() == nil {
		return nil, problemf(http.StatusServiceUnavailable, CodeCatalogNotLoaded, "the catalog is not loaded yet")
	}
	r := requestFrom(ctx)
	b := req.Body
	reg, err := s.d.Scheduler.Register(ctx, scheduler.RegisterRequest{
		EnrollmentToken: r.Header.Get(HeaderEnrollmentToken),
		RefreshToken:    r.Header.Get(HeaderAgentRefreshToken),
		Pool:            b.Pool,
		Host:            b.Host,
		AgentVersion:    b.AgentVersion,
		Capabilities:    b.Capabilities,
		Lending:         b.Lending,
	})
	if err != nil {
		return nil, err
	}
	out := gen.RegisterAgent201JSONResponse{AgentId: reg.AgentID, AgentToken: reg.AgentToken, ExpiresAt: reg.ExpiresAt}
	if reg.RefreshToken != "" {
		rt := reg.RefreshToken
		out.RefreshToken = &rt
	}
	return out, nil
}

// ownAgent checks that the calling agent is id: an agent acts only on itself.
func ownAgent(ctx context.Context, id string) (string, error) {
	p := principal(ctx)
	if p.Kind != auth.KindAgent {
		return "", problemf(http.StatusUnauthorized, CodeUnauthenticated, "an agent token is required")
	}
	if p.AgentID != id {
		return "", problemf(http.StatusForbidden, CodeNotYourAgent, "an agent may only act on itself")
	}
	return p.AgentID, nil
}

// leaseCaller is who calls a lease's task or run route: the calling agent's id and, for a run
// token, its lease's task (else ""). The handler checks that the agent holds the lease, and
// that a run token's task is the route's.
func leaseCaller(ctx context.Context) (agent, task string, err error) {
	switch p := principal(ctx); p.Kind {
	case auth.KindAgent:
		return p.AgentID, "", nil
	case auth.KindRun:
		return p.AgentID, p.Task, nil
	}
	return "", "", problemf(http.StatusUnauthorized, CodeUnauthenticated, "an agent or run token is required")
}

// callingAgent is the calling agent's id.
func callingAgent(ctx context.Context) (string, error) {
	p := principal(ctx)
	if p.Kind != auth.KindAgent {
		return "", problemf(http.StatusUnauthorized, CodeUnauthenticated, "an agent token is required")
	}
	return p.AgentID, nil
}

// UpdateAgent implements gen.StrictServerInterface.
func (s *Server) UpdateAgent(ctx context.Context, req gen.UpdateAgentRequestObject) (gen.UpdateAgentResponseObject, error) {
	id, err := ownAgent(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	u := scheduler.AgentUpdate{Lending: req.Body.Lending}
	if req.Body.Phase != nil {
		u.Phase = *req.Body.Phase
	}
	if req.Body.Until != nil {
		u.Until = *req.Body.Until
	}
	a, err := s.d.Scheduler.UpdateAgent(ctx, id, u)
	if err != nil {
		return nil, err
	}
	return gen.UpdateAgent200JSONResponse(*a), nil
}

// DeregisterAgent implements gen.StrictServerInterface. The agent deregisters itself; an
// admin's delete (a UI admin or the CLI token, agentOrAdmin) also revokes the agent's refresh
// token, so it can't register again without a new enrollment.
func (s *Server) DeregisterAgent(ctx context.Context, req gen.DeregisterAgentRequestObject) (gen.DeregisterAgentResponseObject, error) {
	if k := principal(ctx).Kind; k == auth.KindUser || k == auth.KindCLI {
		if err := s.d.Scheduler.RevokeRefresh(ctx, req.Id); err != nil {
			return nil, err
		}
		if err := s.d.Scheduler.Deregister(ctx, req.Id); err != nil {
			return nil, err
		}
		s.log.InfoContext(ctx, "agent deregistered by an admin", "agent", req.Id, "actor", principal(ctx).ActorID())
		return gen.DeregisterAgent204Response{}, nil
	}
	id, err := ownAgent(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if err := s.d.Scheduler.Deregister(ctx, id); err != nil {
		return nil, err
	}
	return gen.DeregisterAgent204Response{}, nil
}

// LeaseTask implements gen.StrictServerInterface: the long poll. 204 when nothing matched in
// time.
func (s *Server) LeaseTask(ctx context.Context, req gen.LeaseTaskRequestObject) (gen.LeaseTaskResponseObject, error) {
	id, err := ownAgent(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	wait := DefaultLeaseWait
	if req.Params.Wait != nil {
		w := *req.Params.Wait
		if !waitRE.MatchString(w) {
			return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "wait must be whole seconds, like 30s",
				fields: []gen.FieldError{{Field: "wait", Message: "must match ^[0-9]{1,2}s$"}}}
		}
		n, _ := strconv.Atoi(w[:len(w)-1])
		wait = min(time.Duration(n)*time.Second, MaxLeaseWait)
	}
	if s.stopCtx.Err() != nil {
		return gen.LeaseTask204Response{}, nil // shutting down: no new leases
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.stopCtx, cancel)()
	g, err := s.d.Scheduler.Lease(ctx, id, wait)
	if err != nil && g == nil && errors.Is(err, context.Canceled) && s.stopCtx.Err() != nil {
		return gen.LeaseTask204Response{}, nil // the poll was ended by Shutdown
	}
	if err != nil {
		return nil, err
	}
	if g == nil {
		return gen.LeaseTask204Response{}, nil
	}
	l := gen.Lease{
		Lease: gen.LeaseInfo{
			Id: g.Lease.ID, Agent: g.Lease.Agent, Task: g.Lease.Task, ExpiresAt: g.Lease.ExpiresAt,
			HeartbeatEvery: g.Lease.HeartbeatEvery.String(),
		},
		Task: *g.Task,
	}
	if g.Lease.Run != "" {
		run := g.Lease.Run
		l.Lease.Run = &run
	}
	if g.GitCredential != nil {
		l.GitCredential = gitCredential(*g.GitCredential)
	}
	l.ReadCredentials = gitCredentials(g.ReadCredentials)
	if g.RunToken != "" {
		rt := g.RunToken
		l.RunToken = &rt
	}
	return gen.LeaseTask200JSONResponse(l), nil
}

// gitCredential copies a minted credential into the API type field by field: Credential's own
// JSON redacts the token, so it must never be marshalled as is.
func gitCredential(c provider.Credential) gen.GitCredential {
	out := gen.GitCredential{
		Provider:    c.Provider,
		Host:        c.Host,
		Username:    c.Username,
		Token:       c.Token,
		ExpiresAt:   c.ExpiresAt,
		Repo:        c.Repo,
		ReadRepos:   slices.Clone(c.ReadRepos),
		Permissions: maps.Clone(c.Permissions),
	}
	return out
}

// gitCredentials copies minted credentials into the API type (nil when there are none).
func gitCredentials(cs []provider.Credential) []gen.GitCredential {
	if len(cs) == 0 {
		return nil
	}
	out := make([]gen.GitCredential, len(cs))
	for i, c := range cs {
		out[i] = gitCredential(c)
	}
	return out
}

// HeartbeatLease implements gen.StrictServerInterface.
func (s *Server) HeartbeatLease(ctx context.Context, req gen.HeartbeatLeaseRequestObject) (gen.HeartbeatLeaseResponseObject, error) {
	agent, err := callingAgent(ctx)
	if err != nil {
		return nil, err
	}
	hb := scheduler.HeartbeatRequest{Progress: req.Body.Progress}
	if u := req.Body.Usage; u != nil {
		d := &scheduler.UsageDelta{}
		if u.InputTokens != nil {
			d.InputTokens = *u.InputTokens
		}
		if u.OutputTokens != nil {
			d.OutputTokens = *u.OutputTokens
		}
		if u.CostUSD != nil {
			c := float64(*u.CostUSD)
			d.CostUSD = &c
		}
		hb.Usage = d
	}
	resp, err := s.d.Scheduler.Heartbeat(ctx, agent, req.Id, hb)
	if err != nil {
		return nil, err
	}
	out := gen.HeartbeatResponse{ExpiresAt: resp.ExpiresAt, Cancel: resp.Cancel}
	if c := resp.RefreshedGitCredential; c != nil {
		gc := gitCredential(*c)
		out.RefreshedGitCredential = &gc
	}
	out.RefreshedReadCredentials = gitCredentials(resp.RefreshedReadCredentials)
	return gen.HeartbeatLease200JSONResponse(out), nil
}

// CompleteLease implements gen.StrictServerInterface. It goes through Scheduler.Complete (not
// the run controller's), which tops up the usage and ends the lease.
func (s *Server) CompleteLease(ctx context.Context, req gen.CompleteLeaseRequestObject) (gen.CompleteLeaseResponseObject, error) {
	agent, err := callingAgent(ctx)
	if err != nil {
		return nil, err
	}
	if ref := req.Body.TranscriptRef; ref != nil && *ref != "" {
		if err := s.recordTranscriptRef(ctx, agent, req.Id, *ref); err != nil {
			return nil, err
		}
	}
	if _, err := s.d.Scheduler.Complete(ctx, agent, req.Id, req.Body.Result); err != nil {
		return nil, err
	}
	return gen.CompleteLease204Response{}, nil
}

// recordTranscriptRef sets the transcript reference on the lease's open run (best effort: the
// upload already set it; a lease the agent doesn't hold is refused by Complete).
func (s *Server) recordTranscriptRef(ctx context.Context, agent, leaseID, ref string) error {
	var l scheduler.Lease
	if err := s.d.Store.Get(ctx, store.Key{Kind: scheduler.KindLease, Name: leaseID}, &l); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get lease %s: %w", leaseID, err)
	}
	if l.Spec.Agent != agent {
		return nil
	}
	run, err := s.openRun(ctx, l.Spec.Task)
	if err != nil || run == nil {
		return err
	}
	return s.setTranscriptRef(ctx, run.Metadata.Name, ref)
}

// AbandonLease implements gen.StrictServerInterface.
func (s *Server) AbandonLease(ctx context.Context, req gen.AbandonLeaseRequestObject) (gen.AbandonLeaseResponseObject, error) {
	agent, err := callingAgent(ctx)
	if err != nil {
		return nil, err
	}
	ab := scheduler.AbandonRequest{Reason: string(req.Body.Reason)}
	if req.Body.Until != nil {
		ab.Until = *req.Body.Until
	}
	if req.Body.Message != nil {
		ab.Message = *req.Body.Message
	}
	if err := s.d.Scheduler.Abandon(ctx, agent, req.Id, ab); err != nil {
		return nil, err
	}
	return gen.AbandonLease204Response{}, nil
}

// openRun is the task's run that hasn't ended (the newest attempt), or nil.
func (s *Server) openRun(ctx context.Context, taskID string) (*v1alpha1.Run, error) {
	runs, err := s.taskRuns(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var open *v1alpha1.Run
	for _, r := range runs {
		if (r.Status == nil || r.Status.End == "") && (open == nil || r.Spec.Attempt > open.Spec.Attempt) {
			open = r
		}
	}
	return open, nil
}

func (s *Server) taskRuns(ctx context.Context, taskID string) ([]*v1alpha1.Run, error) {
	var runs []*v1alpha1.Run
	if _, err := s.d.Store.List(ctx, v1alpha1.KindRun, store.ListOptions{
		Field: map[string]string{store.FieldRunTask: taskID},
	}, &runs); err != nil {
		return nil, fmt.Errorf("list runs of %s: %w", taskID, err)
	}
	return runs, nil
}
