package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// DefaultRuleIdentity is the factory identity that rule-level notifications and effects act as.
const DefaultRuleIdentity = "author"

// LabelRule is the label of an Effect record made by a rule: the rule's name.
const LabelRule = "factory.ccf.dev/rule"

// applier applies decisions to the store and the providers. It is shared by the rule
// controller and the debounce controller.
type applier struct {
	s        store.Store
	conns    ConnSource
	skills   SkillResolver
	clk      clock.Clock
	log      *slog.Logger
	uiBase   string
	identity string
}

// applyCtx is the state one sequence of decisions is applied to.
type applyCtx struct {
	cat *catalog.Catalog
	ev  v1alpha1.Event
	// wi is updated in place (state, links); the caller writes it once all decisions applied.
	wi  *v1alpha1.WorkItem
	now time.Time
}

// applied is one decision to apply.
type applied struct {
	ruleSet, rule string
	action        rules.ActionIntent
	verdict       v1alpha1.RailVerdict
	// key is the decision's idempotency key: evt/<event>/<rule>/<i>, or dbn/<debounce>/<i>.
	key string
}

// apply applies one decision. A permanent failure (a dispatch that doesn't compile, a template
// that doesn't render, an effect the provider rejects) is returned as a note for the decision
// record; any other error is returned so the reconcile is retried (every step is idempotent).
func (a *applier) apply(ctx context.Context, ac *applyCtx, d applied) (note string, err error) {
	switch d.verdict.Verdict {
	case rules.VerdictAllowed, rules.VerdictDeferred:
	default:
		return "", nil // suppressed or unsupported: recorded, not applied
	}
	act := d.action
	switch {
	case act.Set != nil:
		st := ensureStatus(ac.wi)
		if st.State == nil {
			st.State = map[string]any{}
		}
		st.State[act.Set.Key] = jsonValue(act.Set.Value)
		return "", nil
	case act.Link != nil:
		AddLink(ac.wi, act.Link.Ref, ac.ev.Source)
		return "", nil
	case act.Dispatch != nil:
		return a.dispatch(ctx, ac, d)
	case act.Cancel != nil:
		return "", a.cancel(ctx, ac, act.Cancel.TaskSpec)
	case act.Notify != nil:
		return a.notify(ctx, ac, d)
	case act.Effect != nil:
		return a.effect(ctx, ac, d)
	case act.Debounce != nil:
		return "", a.debounce(ctx, ac, d)
	}
	return "", nil
}

func ensureStatus(wi *v1alpha1.WorkItem) *v1alpha1.WorkItemStatus {
	if wi.Status == nil {
		wi.Status = &v1alpha1.WorkItemStatus{}
	}
	return wi.Status
}

// AddLink adds ref to the work item unless it is linked already (same provider and ref), and
// reports whether it did. A ref without a provider is on the primary link's provider (else on
// fallback); one without a kind gets guessKind's. The new link is primary only when it is the
// item's first.
func AddLink(wi *v1alpha1.WorkItem, ref v1alpha1.ObjectRef, fallback string) bool {
	p := ref.Provider
	if p == "" {
		if l, ok := primaryLink(wi.Spec.Links); ok {
			p = l.Provider
		} else {
			p = fallback
		}
	}
	if slices.ContainsFunc(wi.Spec.Links, func(l v1alpha1.Link) bool { return l.Provider == p && l.Ref == ref.Ref }) {
		return false
	}
	kind := ref.Kind
	if kind == "" {
		kind = guessKind(ref.Ref)
	}
	wi.Spec.Links = append(wi.Spec.Links, v1alpha1.Link{Provider: p, Ref: ref.Ref, Kind: kind, Primary: len(wi.Spec.Links) == 0})
	return true
}

// maxLinkCAS bounds LinkObject's compare-and-swap retries.
const maxLinkCAS = 10

// LinkObject adds ref to the stored work item id (AddLink's rules) outside a rule evaluation:
// the Effector links the object an effect created. It re-reads and retries an update that
// lost to a concurrent writer (the store's optimistic concurrency), writes nothing when the
// ref is linked already, and is a no-op for a work item that's gone. It reports whether it
// added the link.
func LinkObject(ctx context.Context, s store.Store, id string, ref v1alpha1.ObjectRef, fallback string) (bool, error) {
	for range maxLinkCAS {
		var wi v1alpha1.WorkItem
		switch err := s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); {
		case errors.Is(err, store.ErrNotFound):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("get work item %s: %w", id, err)
		}
		if !AddLink(&wi, ref, fallback) {
			return false, nil
		}
		err := s.Update(ctx, &wi)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, store.ErrNotFound):
			return false, nil
		case !errors.Is(err, store.ErrConflict):
			return false, fmt.Errorf("link %s to work item %s: %w", ref.Ref, id, err)
		}
	}
	return false, fmt.Errorf("link %s to work item %s: %d conflicting updates: %w", ref.Ref, id, maxLinkCAS, store.ErrConflict)
}

// guessKind is "issue" for an "owner/name#n" ref and "" otherwise (the provider knows).
func guessKind(ref string) string {
	if _, n, ok := strings.Cut(ref, "#"); ok {
		if _, err := strconv.Atoi(n); err == nil {
			return provider.KindIssue
		}
	}
	return ""
}

func (a *applier) dispatch(ctx context.Context, ac *applyCtx, d applied) (string, error) {
	taskID := objectID("tsk", d.key)
	var existing v1alpha1.Task
	switch err := a.s.Get(ctx, store.Key{Kind: v1alpha1.KindTask, Name: taskID}, &existing); {
	case err == nil:
		return "", nil // created by an earlier attempt of this reconcile
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("get task %s: %w", taskID, err)
	}
	deferred := ""
	if d.verdict.Verdict == rules.VerdictDeferred {
		deferred = ReasonBudget
	}
	task, prompt, err := compileTask(ctx, compileInput{
		cat: ac.cat, d: d.action.Dispatch, wi: ac.wi, ev: ac.ev, ruleSet: d.ruleSet, rule: d.rule,
		taskID: taskID, deferred: deferred, now: ac.now, kindOf: a.objectKind,
	}, a.skills)
	if errors.Is(err, ErrInvalidDispatch) {
		return err.Error(), nil
	}
	if err != nil {
		return "", err
	}
	if err := a.s.Create(ctx, prompt); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("store prompt of %s: %w", taskID, err)
	}
	if err := a.s.Create(ctx, task); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("create task %s: %w", taskID, err)
	}
	a.log.InfoContext(ctx, "task dispatched", "task", taskID, "taskSpec", task.Spec.TaskSpec.Name, "queue", task.Spec.Queue,
		"workItem", ac.wi.Metadata.Name, "rule", d.rule, "reason", deferred)
	return "", nil
}

// objectKind resolves an "owner/name#n" ref's kind (issue or PR) by observing it, with no kind,
// on its provider. A retryable provider error is returned (the reconcile retries). Without a
// connection, or when the object is gone or the provider refuses it, the ref is guessed an
// issue, as AddLink does: its content was unreadable anyway, and a gone item reads as gone.
func (a *applier) objectKind(ctx context.Context, prov, ref string) (string, error) {
	if a.conns == nil {
		return provider.KindIssue, nil
	}
	conn, ok := a.conns.Conn(prov)
	if !ok {
		return provider.KindIssue, nil
	}
	obs, err := conn.Observe(ctx, v1alpha1.ObjectRef{Provider: prov, Ref: ref})
	switch {
	case err != nil && (errors.Is(err, provider.ErrRetryable) || ctx.Err() != nil):
		return "", fmt.Errorf("resolve the kind of %s on %s: %w", ref, prov, err)
	case err != nil:
		if !isNotFound(err) {
			a.log.WarnContext(ctx, "context ref kind unresolved: guessed an issue", "provider", prov, "ref", ref, "err", err)
		}
		return provider.KindIssue, nil
	case obs.Ref.Kind == provider.KindPR:
		return provider.KindPR, nil
	}
	return provider.KindIssue, nil
}

// cancel moves the work item's active tasks of a TaskSpec to Cancelling (the run controller
// cancels a task no agent holds at once). Tasks this event created are left alone, so a
// retried cancel-then-dispatch doesn't cancel its own dispatch.
func (a *applier) cancel(ctx context.Context, ac *applyCtx, spec string) error {
	tasks, err := listTasks(ctx, a.s, ac.wi.Metadata.Name)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Spec.TaskSpec.Name != spec || (t.Metadata.Provenance != nil && t.Metadata.Provenance.Event == ac.ev.ID) {
			continue
		}
		switch taskPhase(t) {
		case v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning:
		default:
			continue
		}
		if t.Status == nil {
			t.Status = &v1alpha1.TaskStatus{}
		}
		t.Status.Phase = v1alpha1.TaskCancelling
		t.Status.Reason = ReasonCancelled
		if err := a.s.Update(ctx, t); err != nil {
			return fmt.Errorf("cancel task %s: %w", t.Metadata.ID, err)
		}
		a.log.InfoContext(ctx, "task cancelling", "task", t.Metadata.ID, "workItem", ac.wi.Metadata.Name)
	}
	return nil
}

func (a *applier) notify(ctx context.Context, ac *applyCtx, d applied) (string, error) {
	n := d.action.Notify
	reqs, err := notifyRequests(ac.cat, n.Template, n.To, ac.wi, templateData(ac.ev, ac.wi, a.uiBase), a.identity)
	if err != nil {
		return err.Error(), nil
	}
	var notes []string
	for i, r := range reqs {
		r.req.IdempotencyKey = d.key + "/" + strconv.Itoa(i)
		note, err := a.send(ctx, ac, d, r.provider, r.req)
		if err != nil {
			return "", err
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return strings.Join(notes, "; "), nil
}

func (a *applier) effect(ctx context.Context, ac *applyCtx, d applied) (string, error) {
	e := d.action.Effect
	primary, ok := primaryLink(ac.wi.Spec.Links)
	target := v1alpha1.ObjectRef{Provider: primary.Provider, Kind: primary.Kind, Ref: primary.Ref}
	switch e.Target {
	case "", "primary":
		if !ok {
			return "effect " + string(e.Op) + ": the work item has no primary link", nil
		}
	default:
		target = v1alpha1.ObjectRef{Provider: primary.Provider, Ref: e.Target}
		for _, l := range ac.wi.Spec.Links {
			if l.Ref == e.Target {
				target = v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref}
				break
			}
		}
		if target.Provider == "" {
			target.Provider = ac.ev.Source
		}
	}
	if e.Op == v1alpha1.OpCreateIssue && strings.Contains(target.Ref, "#") {
		target = v1alpha1.ObjectRef{Provider: target.Provider, Ref: repoOf(target.Ref)}
	}
	return a.send(ctx, ac, d, target.Provider, provider.EffectRequest{
		Op: e.Op, Target: target, Args: e.Args, Identity: a.identity, IdempotencyKey: d.key,
	})
}

func repoOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	return repo
}

// send applies one effect through its provider and records it as an Effect. A retryable
// provider error is returned (the reconcile retries; the idempotency key keeps it single);
// any other provider error is a note.
func (a *applier) send(ctx context.Context, ac *applyCtx, d applied, prov string, req provider.EffectRequest) (string, error) {
	conn, ok := a.conns.Conn(prov)
	if !ok {
		return fmt.Sprintf("%s: no connection for provider %q", req.Op, prov), nil
	}
	res, err := conn.Apply(ctx, req)
	if err != nil && (errors.Is(err, provider.ErrRetryable) || ctx.Err() != nil) {
		return "", fmt.Errorf("apply %s on %s: %w", req.Op, req.Target.Ref, err)
	}
	rec := &v1alpha1.Effect{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
		Metadata: v1alpha1.ObjectMeta{Name: objectID("eff", req.IdempotencyKey), Labels: map[string]string{LabelRule: d.rule}},
		Spec: v1alpha1.EffectSpec{
			Op: req.Op, Target: req.Target, IdempotencyKey: req.IdempotencyKey, Identity: req.Identity,
			Request: req.Args,
		},
		Status: &v1alpha1.EffectStatus{At: ac.now, Result: &v1alpha1.EffectResult{URL: res.URL}},
	}
	note := ""
	if err != nil {
		rec.Status.Result = &v1alpha1.EffectResult{Error: err.Error()}
		note = fmt.Sprintf("%s on %s failed: %v", req.Op, req.Target.Ref, err)
		a.log.WarnContext(ctx, "rule effect failed", "op", req.Op, "target", req.Target.Ref, "rule", d.rule, "err", err)
	}
	if err := a.s.Create(ctx, rec); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("record effect %s: %w", rec.Metadata.Name, err)
	}
	return note, nil
}

// debounce stores the held actions; a pending one with the same key wins (first wins).
func (a *applier) debounce(ctx context.Context, ac *applyCtx, d applied) error {
	db := d.action.Debounce
	rec := &DebounceRecord{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindDebounce},
		Metadata: v1alpha1.ObjectMeta{Name: objectID("dbn", db.Key)},
		Spec: DebounceSpec{
			Key: db.Key, WorkItem: ac.wi.Metadata.Name, Event: ac.ev.ID, RuleSet: d.ruleSet, Rule: d.rule,
			Until: db.Until, Then: db.Then,
		},
	}
	if err := a.s.Create(ctx, rec); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("store debounce %s: %w", db.Key, err)
	}
	return nil
}

// appendDecisions appends records to the work item, keeping the newest MaxDecisions.
func appendDecisions(wi *v1alpha1.WorkItem, recs []v1alpha1.Decision) {
	if len(recs) == 0 {
		return
	}
	st := ensureStatus(wi)
	st.Decisions = append(st.Decisions, recs...)
	if over := len(st.Decisions) - v1alpha1.MaxDecisions; over > 0 {
		st.Decisions = slices.Clone(st.Decisions[over:])
	}
}
