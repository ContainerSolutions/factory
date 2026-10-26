package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// DefaultLockTTL is the lease of a work item lock ("wi/<id>"). A reconcile holds it for one
// event; an expired lock is reclaimable, so a stuck holder can't block the item for long.
const DefaultLockTTL = 2 * time.Minute

// RuleOptions configure the rule controller. Zero fields take the defaults.
type RuleOptions struct {
	// Workers reconciles that many events at once (different work items run in parallel;
	// one work item's events are serialised by its lock). Default 2.
	Workers int
	// Clock stamps decisions and is the engine's `now`. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Cache is the observation cache, shared with the work-item controller. Default a new
	// one with DefaultObservationTTL.
	Cache *ObservationCache
	// KillSwitches are the pause flags. Default StoreKillSwitches over the controller's store
	// (the flags the pause/resume commands write).
	KillSwitches KillSwitches
	// Skills resolves TaskSpec skill refs. Default KeepSkills.
	Skills SkillResolver
	// RunCap is the per-item task cap. Default DefaultRunCap.
	RunCap int
	// LockTTL is the work item lock's lease. Default DefaultLockTTL.
	LockTTL time.Duration
	// UIBaseURL is the Factory UI's public URL, for templates' links.ui.
	UIBaseURL string
	// Identity is the factory identity of rule-level notifications and effects. Default
	// DefaultRuleIdentity.
	Identity string
}

func (o RuleOptions) withDefaults(conns ConnSource) RuleOptions {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Cache == nil {
		o.Cache = NewObservationCache(conns, o.Clock, 0)
	}
	if o.KillSwitches == nil {
		o.KillSwitches = NoKillSwitches{}
	}
	if o.Skills == nil {
		o.Skills = KeepSkills{}
	}
	if o.LockTTL <= 0 {
		o.LockTTL = DefaultLockTTL
	}
	if o.Identity == "" {
		o.Identity = DefaultRuleIdentity
	}
	return o
}

// RuleController turns Events into decisions (controllers.md §rule): for each unprocessed
// Event it finds or creates the work item, builds its WorkItemView, runs the engine, applies
// the decisions in order under the work item's lock, appends the decision records and marks
// the Event processed. Its key is the event id.
type RuleController struct {
	s      store.Store
	cat    CatalogProvider
	engine *rules.Engine
	opts   RuleOptions
	apply  *applier
	log    *slog.Logger
	ctrl   *Controller
}

// NewRuleController returns the rule controller, watching the store's Events.
func NewRuleController(s store.Store, cat CatalogProvider, conns ConnSource, opts RuleOptions) (*RuleController, error) {
	if opts.KillSwitches == nil {
		opts.KillSwitches = StoreKillSwitches{Store: s}
	}
	opts = opts.withDefaults(conns)
	r := &RuleController{
		s:      s,
		cat:    cat,
		engine: &rules.Engine{Clock: opts.Clock},
		opts:   opts,
		log:    opts.Logger.With("controller", "rule"),
		apply: &applier{
			s: s, conns: conns, skills: opts.Skills, clk: opts.Clock,
			log: opts.Logger.With("controller", "rule"), uiBase: opts.UIBaseURL, identity: opts.Identity,
		},
	}
	r.ctrl = NewController("rule", r, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := r.ctrl.Watch(s, v1alpha1.KindEvent, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return r, nil
}

// Controller returns the underlying controller (its queue, for tests and the manager).
func (r *RuleController) Controller() *Controller { return r.ctrl }

// Run runs the controller until ctx is done.
func (r *RuleController) Run(ctx context.Context) error { return r.ctrl.Run(ctx) }

// Reconcile implements Reconciler for one event id.
func (r *RuleController) Reconcile(ctx context.Context, id string) (Result, error) {
	var ev v1alpha1.Event
	err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: id}, &ev)
	if errors.Is(err, store.ErrNotFound) {
		// The event aged out: drop its processed mark with it.
		if err := r.s.Delete(ctx, store.Key{Kind: KindEventStatus, Name: id}); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete status of event %s: %w", id, err)
		}
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get event %s: %w", id, err)
	}
	if done, err := r.processed(ctx, id); err != nil || done {
		return Result{}, err
	}
	cat := r.cat.Current()
	if cat == nil {
		return Result{}, errors.New("no catalog loaded yet")
	}
	wiID, err := r.workItemFor(ctx, cat, ev)
	if err != nil {
		return Result{}, err
	}
	if wiID == "" {
		return Result{}, r.markProcessed(ctx, ev.ID, "", 0)
	}
	return Result{}, r.process(ctx, cat, ev, wiID)
}

func (r *RuleController) processed(ctx context.Context, id string) (bool, error) {
	var st EventStatus
	err := r.s.Get(ctx, store.Key{Kind: KindEventStatus, Name: id}, &st)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("get status of event %s: %w", id, err)
	}
}

// markProcessed marks event id processed for workItem ("" for none), with how many decision
// records the processing appended there.
func (r *RuleController) markProcessed(ctx context.Context, id, workItem string, recorded int) error {
	st := &EventStatus{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindEventStatus},
		Metadata: v1alpha1.ObjectMeta{Name: id},
		WorkItem: workItem, ProcessedAt: r.opts.Clock.Now(), Recorded: &recorded,
	}
	if err := r.s.Create(ctx, st); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("mark event %s processed: %w", id, err)
	}
	return nil
}

// eventLink is the link a provider event is about: its object, else its repo.
func eventLink(ev v1alpha1.Event) (v1alpha1.Link, bool) {
	if o := ev.Data.Object; o != nil && o.Ref != "" {
		p := o.Provider
		if p == "" {
			p = ev.Source
		}
		return v1alpha1.Link{Provider: p, Ref: o.Ref, Kind: o.Kind, Primary: true}, true
	}
	if ev.Data.Repo != "" {
		return v1alpha1.Link{Provider: ev.Source, Ref: ev.Data.Repo, Kind: "repo", Primary: true}, true
	}
	return v1alpha1.Link{}, false
}

// workItemFor finds the event's work item, or creates one when at least one rule would fire
// for the event (asked with the new item's view: no state, its object observed, permissive
// rails). "" means no work item: the event changes nothing.
//
// For a new work item, nothing is observed unless it can matter: an event no rule could fire
// for (its repo in no RuleSet's scope, say) is dropped first, and the object is observed only
// when a where that could fire reads item. A provider refusing that observation is Permanent:
// the event is marked processed, with no work item.
func (r *RuleController) workItemFor(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event) (string, error) {
	if ev.Source == v1alpha1.FactoryProvider {
		if ev.WorkItem == "" {
			return "", nil
		}
		var wi v1alpha1.WorkItem
		switch err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: ev.WorkItem}, &wi); {
		case errors.Is(err, store.ErrNotFound):
			r.log.InfoContext(ctx, "factory event for a missing work item", "event", ev.ID, "workItem", ev.WorkItem)
			return "", nil
		case err != nil:
			return "", fmt.Errorf("get work item %s: %w", ev.WorkItem, err)
		}
		return ev.WorkItem, nil
	}
	link, hasLink := eventLink(ev)
	if hasLink {
		var found []*v1alpha1.WorkItem
		if _, err := r.s.List(ctx, v1alpha1.KindWorkItem, store.ListOptions{
			Field: map[string]string{store.FieldWorkItemLink: store.LinkFieldValue(link.Provider, link.Ref)},
		}, &found); err != nil {
			return "", fmt.Errorf("find work item of %s: %w", link.Ref, err)
		}
		if len(found) > 0 {
			return found[0].Metadata.Name, nil // name order: the oldest id wins if two claim it
		}
	}
	// A deterministic id: two events about the same new object create one work item.
	seed := "event/" + ev.ID
	if hasLink {
		seed = "link/" + store.LinkFieldValue(link.Provider, link.Ref)
	}
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: objectID("wi", seed)},
		Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	if hasLink {
		wi.Spec.Links = []v1alpha1.Link{link}
	}
	if a := ev.Data.Actor; a != nil {
		rep := *a
		wi.Status.Reporter = &rep
	}
	// Could any rule fire at all? Scope, on and providers need no observation.
	unobserved := buildView(wi, nil, observations{})
	if !rules.CouldFire(cat, ev, unobserved) {
		r.skipped(ctx, ev, link, "no rule could fire for the event; skipped")
		return "", nil
	}
	// Would any rule fire? Ask with the work item as it would be: no state, no runs, its
	// object observed when a where reads item (item.pr and the like), and permissive rails.
	view := unobserved
	if rules.ReadsItem(cat, ev, unobserved) {
		obs, err := observeLinks(ctx, r.opts.Cache, wi.Spec.Links)
		if err != nil {
			return "", err
		}
		if ferr := obs.forbiddenErr(wi.Spec.Links); ferr != nil {
			if err := r.markProcessed(ctx, ev.ID, "", 0); err != nil {
				return "", err
			}
			return "", Permanent(fmt.Errorf("event %s: %w", ev.ID, ferr))
		}
		view = buildView(wi, nil, obs)
	}
	eval, err := r.engine.Evaluate(ctx, cat, ev, view, rules.AllowAll)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(eval.Matches, rules.Match.Fired) {
		r.skipped(ctx, ev, link, "no rule fires for the event; skipped")
		return "", nil
	}
	if err := r.s.Create(ctx, wi); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("create work item: %w", err)
	}
	r.log.InfoContext(ctx, "work item created", "workItem", wi.Metadata.Name, "event", ev.ID, "link", link.Ref)
	return wi.Metadata.Name, nil
}

// skipped logs a provider event that no work item links and no rule fires for: at Debug, but
// at Info when the factory caused it. The objects the factory creates are linked to their
// task's work item when the Effector applies them, so a factory event about an object no work
// item links is unexpected (an unlinked PR's review never dispatched, say) and should show.
// Only the event's id, type and ref are logged, no provider text.
func (r *RuleController) skipped(ctx context.Context, ev v1alpha1.Event, link v1alpha1.Link, msg string) {
	if a := ev.Data.Actor; a != nil && a.IsFactory {
		r.log.InfoContext(ctx, "factory event about an object no work item links; skipped", "event", ev.ID, "type", ev.Type, "ref", link.Ref)
		return
	}
	r.log.DebugContext(ctx, msg, "event", ev.ID, "type", ev.Type, "link", link.Ref)
}

// observeItem observes a work item's links for an evaluation. A link the provider won't show
// is evaluated as gone, so the other links still count; it is reported once at WARN (until it
// observes again), not on every evaluation.
func (r *RuleController) observeItem(ctx context.Context, wiID string, links []v1alpha1.Link) (observations, error) {
	obs, err := observeLinks(ctx, r.opts.Cache, links)
	if err != nil {
		return observations{}, err
	}
	for _, l := range links {
		if ferr := obs.forbidden[cacheKey(l)]; ferr != nil && r.opts.Cache.firstForbidden(l) {
			r.log.WarnContext(ctx, "link observation forbidden; evaluating it as gone", "workItem", wiID, "link", l.Provider+"/"+l.Ref, "err", ferr)
		}
	}
	return obs, nil
}

// process evaluates and applies one event for one work item under the item's lock.
func (r *RuleController) process(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event, wiID string) error {
	return withItemLock(ctx, r.s, wiID, r.opts.LockTTL, func(ctx context.Context) error {
		_, err := r.processLocked(ctx, cat, ev, wiID, false)
		return err
	})
}

// processLocked is process with the work item's lock held. It returns the decision records it
// appended.
//
// On a replay, a rule that has an earlier fired record for the event on the item is never
// applied again, whatever changed since (a rail that suppressed it then, an action the catalog
// added or moved, which would shift its keys), and its new evaluation isn't recorded: a set,
// link or cancel it made isn't redone over what happened since, and no dispatch is made twice.
// The other rules are applied with the event's keys (evt/<event>/<rule>/<i>), and their records
// that differ from the event's earlier ones (sameDecision) are appended, marked Replay. When
// the event was processed for this item with records but none remain (they aged out of
// MaxDecisions), what already fired is unknown: ErrReplayHistoryGone, nothing applied.
func (r *RuleController) processLocked(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event, wiID string, replay bool) ([]v1alpha1.Decision, error) {
	var wi v1alpha1.WorkItem
	switch err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, &wi); {
	case errors.Is(err, store.ErrNotFound):
		return nil, r.markProcessed(ctx, ev.ID, "", 0) // deleted (retention) since it was found
	case err != nil:
		return nil, fmt.Errorf("get work item %s: %w", wiID, err)
	}
	var earlier []v1alpha1.Decision
	if replay {
		if wi.Status != nil {
			for _, d := range wi.Status.Decisions {
				if d.Event == ev.ID {
					earlier = append(earlier, d)
				}
			}
		}
		if err := r.checkHistory(ctx, ev.ID, wiID, earlier); err != nil {
			return nil, err
		}
	}
	tasks, err := listTasks(ctx, r.s, wiID)
	if err != nil {
		return nil, err
	}
	runs, err := runsBySpec(ctx, r.s, tasks)
	if err != nil {
		return nil, err
	}
	obs, err := r.observeItem(ctx, wiID, wi.Spec.Links)
	if err != nil {
		return nil, err
	}
	now := r.opts.Clock.Now()
	rails, err := newStoreRails(ctx, r.s, r.opts.KillSwitches, railsInput{
		cat: cat, event: ev.ID, workItem: &wi, tasks: tasks, obs: obs, runCap: r.opts.RunCap, now: now,
	})
	if err != nil {
		return nil, err
	}
	eval, err := r.engine.Evaluate(ctx, cat, ev, buildView(&wi, runs, obs), rails)
	if err != nil {
		return nil, err
	}

	recs := eval.Records(now)
	// fired are the rules that already fired for the event on this item (by name, as the
	// keys are): a replay never applies them again. Empty outside a replay.
	fired := map[string]bool{}
	for _, d := range earlier {
		if d.Fired {
			fired[d.Rule] = true
		}
	}

	ac := &applyCtx{cat: cat, ev: ev, wi: &wi, now: now}
	notes := map[[2]string][]string{}
	perRule := map[string]int{}
	for _, d := range eval.Decisions {
		i := perRule[d.Rule]
		perRule[d.Rule]++
		k := [2]string{d.RuleSet, d.Rule}
		if fired[d.Rule] {
			continue
		}
		note, err := r.apply.apply(ctx, ac, applied{
			ruleSet: d.RuleSet, rule: d.Rule, action: d.Action, verdict: d.Rail,
			key: fmt.Sprintf("evt/%s/%s/%d", ev.ID, d.Rule, i),
		})
		if err != nil {
			return nil, fmt.Errorf("event %s, rule %s: %s: %w", ev.ID, d.Rule, d.Action, err)
		}
		if note != "" {
			notes[k] = append(notes[k], d.Action.String()+": "+note)
		}
	}
	kept := recs[:0]
	for _, rec := range recs {
		k := [2]string{rec.RuleSet, rec.Rule}
		if fired[rec.Rule] || slices.ContainsFunc(earlier, func(d v1alpha1.Decision) bool { return sameDecision(d, rec) }) {
			continue
		}
		if n := notes[k]; len(n) > 0 {
			rec.Reason = strings.TrimPrefix(rec.Reason+"; apply failed: "+strings.Join(n, "; "), "; ")
		}
		rec.Replay = replay
		kept = append(kept, rec)
	}
	appendDecisions(&wi, kept)
	if err := r.s.Update(ctx, &wi); err != nil {
		return nil, fmt.Errorf("update work item %s: %w", wiID, err)
	}
	r.log.DebugContext(ctx, "event applied", "event", ev.ID, "type", ev.Type, "workItem", wiID, "decisions", len(eval.Decisions), "replay", replay)
	if replay {
		return kept, r.markReplayed(ctx, ev.ID, wiID, len(kept))
	}
	return kept, r.markProcessed(ctx, ev.ID, wiID, len(kept))
}

// checkHistory refuses a replay whose earlier records on the item are gone: the event's mark
// names this item and says it recorded something (or predates the count), but no record of
// the event remains there (ErrReplayHistoryGone). An event processed for no item, another
// item, or with no record, has nothing to lose.
func (r *RuleController) checkHistory(ctx context.Context, id, wiID string, earlier []v1alpha1.Decision) error {
	if len(earlier) > 0 {
		return nil
	}
	var st EventStatus
	switch err := r.s.Get(ctx, store.Key{Kind: KindEventStatus, Name: id}, &st); {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("get status of event %s: %w", id, err)
	}
	if st.WorkItem != wiID || (st.Recorded != nil && *st.Recorded == 0) {
		return nil
	}
	return fmt.Errorf("replay event %s on work item %s: %w", id, wiID, ErrReplayHistoryGone)
}

// ErrReplayHistoryGone is a replay of an event whose earlier decision records aged out of its
// work item: which rules already fired is unknown, so nothing is replayed.
var ErrReplayHistoryGone = errors.New("the event's earlier decisions are gone from its work item")

// sameDecision reports whether two records of one event say the same: the same rule, fired
// or not, with the same actions, rail verdicts and reason. The inputs and the time may differ.
func sameDecision(a, b v1alpha1.Decision) bool {
	if a.RuleSet != b.RuleSet || a.Rule != b.Rule || a.Fired != b.Fired || a.Reason != b.Reason ||
		!slices.Equal(a.Actions, b.Actions) || len(a.Rails) != len(b.Rails) {
		return false
	}
	for i := range a.Rails {
		if a.Rails[i].Rail != b.Rails[i].Rail || a.Rails[i].Verdict != b.Rails[i].Verdict {
			return false
		}
	}
	return true
}

// ReplayResult is what a replay recorded: the event's work item ("" when no rule fires for
// it) and the decision records it appended there.
type ReplayResult struct {
	WorkItem  string
	Decisions []v1alpha1.Decision
}

// Replay evaluates the stored event id again, with its own id, against the live catalog and
// its work item as it is now (creating the item when a rule now fires for an event that had
// none), and applies it under the item's lock, as processLocked does on a replay: only the
// rules with no earlier fired record for the event act, and their new records are appended
// marked Replay. A missing event is store.ErrNotFound; with no catalog loaded yet it is
// ErrNoCatalog; an event whose earlier records aged out is ErrReplayHistoryGone.
func (r *RuleController) Replay(ctx context.Context, id string) (ReplayResult, error) {
	var ev v1alpha1.Event
	if err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: id}, &ev); err != nil {
		return ReplayResult{}, fmt.Errorf("get event %s: %w", id, err)
	}
	cat := r.cat.Current()
	if cat == nil {
		return ReplayResult{}, ErrNoCatalog
	}
	wiID, err := r.workItemFor(ctx, cat, ev)
	if err != nil {
		return ReplayResult{}, err
	}
	if wiID == "" {
		return ReplayResult{}, r.markProcessed(ctx, ev.ID, "", 0)
	}
	var recs []v1alpha1.Decision
	err = withItemLock(ctx, r.s, wiID, r.opts.LockTTL, func(ctx context.Context) error {
		var err error
		recs, err = r.processLocked(ctx, cat, ev, wiID, true)
		return err
	})
	if err != nil {
		return ReplayResult{}, err
	}
	return ReplayResult{WorkItem: wiID, Decisions: recs}, nil
}

// markReplayed marks a replayed event processed for workItem, adding the replay's records to
// its count: a mark that names no work item, or another, now names this one (the count
// restarts), so the item's timeline finds the event.
func (r *RuleController) markReplayed(ctx context.Context, id, workItem string, recorded int) error {
	for range maxReplayCAS {
		var st EventStatus
		switch err := r.s.Get(ctx, store.Key{Kind: KindEventStatus, Name: id}, &st); {
		case errors.Is(err, store.ErrNotFound):
			return r.markProcessed(ctx, id, workItem, recorded)
		case err != nil:
			return fmt.Errorf("get status of event %s: %w", id, err)
		}
		n := recorded
		if st.WorkItem == workItem && st.Recorded != nil {
			n += *st.Recorded
		}
		if st.WorkItem == workItem && recorded == 0 {
			return nil // nothing new (an unknown count stays unknown)
		}
		st.WorkItem, st.Recorded = workItem, &n
		err := r.s.Update(ctx, &st)
		if !errors.Is(err, store.ErrConflict) {
			if err != nil {
				return fmt.Errorf("mark event %s processed: %w", id, err)
			}
			return nil
		}
	}
	return fmt.Errorf("mark event %s processed: %w", id, store.ErrConflict)
}

// maxReplayCAS bounds markReplayed's compare-and-swap retries.
const maxReplayCAS = 10

// ErrNoCatalog is a call that needs the live catalog before the first one has loaded.
var ErrNoCatalog = errors.New("no catalog loaded yet")

// DryRun evaluates ev as the rule controller would, but applies nothing and writes nothing to
// the store (the API's dry-run): against the work item wiID as it is now (state, runs,
// observations and the real rails, kill switches included), or, when wiID is empty, against
// the work item the event would create (its object observed, no state). A missing work item
// is store.ErrNotFound.
func (r *RuleController) DryRun(ctx context.Context, ev v1alpha1.Event, wiID string) (rules.Evaluation, error) {
	cat := r.cat.Current()
	if cat == nil {
		return rules.Evaluation{}, errors.New("no catalog loaded yet")
	}
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	var (
		existing *v1alpha1.WorkItem
		tasks    []*v1alpha1.Task
		runs     map[string][]rules.RunSummary
	)
	if wiID != "" {
		if err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, wi); err != nil {
			return rules.Evaluation{}, fmt.Errorf("get work item %s: %w", wiID, err)
		}
		existing = wi
		var err error
		if tasks, err = listTasks(ctx, r.s, wiID); err != nil {
			return rules.Evaluation{}, err
		}
		if runs, err = runsBySpec(ctx, r.s, tasks); err != nil {
			return rules.Evaluation{}, err
		}
	} else if link, ok := eventLink(ev); ok {
		wi.Spec.Links = []v1alpha1.Link{link}
	}
	obs, err := r.observeItem(ctx, wiID, wi.Spec.Links)
	if err != nil {
		return rules.Evaluation{}, err
	}
	rails, err := newStoreRails(ctx, r.s, r.opts.KillSwitches, railsInput{
		cat: cat, event: ev.ID, workItem: existing, tasks: tasks, obs: obs, runCap: r.opts.RunCap, now: r.opts.Clock.Now(),
	})
	if err != nil {
		return rules.Evaluation{}, err
	}
	return r.engine.Evaluate(ctx, cat, ev, buildView(wi, runs, obs), rails)
}
