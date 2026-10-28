package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/store"
)

// Work-item controller defaults.
const (
	// DefaultResyncInterval is how often a work item's links are re-observed when their
	// Provider sets no resync.every.
	DefaultResyncInterval = 10 * time.Minute
)

// KindLinkObservations holds a work item's resync baselines (internal store kind, no API, no
// schema): per link, the digest of its last observation, so a new leader, or one after a
// restart, compares with the last one any leader saw (P3 §Several dispatchers). It is named
// by the work item id, written only when a digest changes, and deleted with the item; the
// leader's sweep deletes one whose work item is gone. It holds digests and link refs only.
const KindLinkObservations = "LinkObservations"

// LinkObservations is a stored KindLinkObservations.
type LinkObservations struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	// Digests maps each link ("<provider>/<ref>") to the digest of its last observation
	// ("gone" when the object no longer existed).
	Digests map[string]string `json:"digests"`
}

var _ store.MetaObject = (*LinkObservations)(nil)

// GetName returns metadata.name (the work item id).
func (o *LinkObservations) GetName() string { return o.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (o *LinkObservations) GetObjectMeta() *v1alpha1.ObjectMeta { return &o.Metadata }

func observationsKey(id string) store.Key { return store.Key{Kind: KindLinkObservations, Name: id} }

// WorkItemOptions configure the work-item controller. Zero fields take the defaults.
type WorkItemOptions struct {
	Workers int
	// Clock times resyncs and retention. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Cache is the observation cache, shared with the rule controller (resync refreshes it).
	// Default a new one.
	Cache *ObservationCache
	// Retention: a work item whose links are all closed is deleted this long after its last
	// activity (`--retention`). Default store.DefaultRetention (90 days).
	Retention time.Duration
	// ResyncInterval applies when a Provider has no resync.every. Default 10 m.
	ResyncInterval time.Duration
	// Jitter picks the delay of an item's first resync in this process, when a leader before
	// it stored the item's baselines: from 0 up to the interval, so a new leader spreads its
	// checks over the interval. Default uniform (math/rand/v2).
	Jitter func(interval time.Duration) time.Duration
	// LockTTL is the work item lock's lease. Default DefaultLockTTL.
	LockTTL time.Duration
}

// WorkItemController keeps work items and their provider objects in line
// (controllers.md §work-item). Per work item (its key is the id) it:
//
//   - projects state, counters and links onto every linked object (Conn.Project) when the
//     projection changed;
//   - re-observes the links every resync interval (refreshing the observation cache) and
//     emits factory.workitem.resynced when an observation changed out of band, against the
//     baselines stored in its LinkObservations;
//   - deletes the item once all its links are closed and its last activity is older than the
//     retention (the store never sweeps WorkItems).
type WorkItemController struct {
	s       store.Store
	cat     CatalogProvider
	conns   ConnSource
	factory *FactoryProvider
	opts    WorkItemOptions
	log     *slog.Logger
	ctrl    *Controller

	// Caches that only save work (a cold one costs a provider call, never a wrong result).
	mu         sync.Mutex
	projected  map[string]string    // wi|link → digest of the last projection written
	lastResync map[string]time.Time // wi → when its links were last observed (or are due)
}

// NewWorkItemController returns the work-item controller, watching WorkItems.
func NewWorkItemController(s store.Store, cat CatalogProvider, conns ConnSource, factory *FactoryProvider, opts WorkItemOptions) (*WorkItemController, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Cache == nil {
		opts.Cache = NewObservationCache(conns, opts.Clock, 0)
	}
	if opts.Retention <= 0 {
		opts.Retention = store.DefaultRetention
	}
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	if opts.Jitter == nil {
		opts.Jitter = func(d time.Duration) time.Duration { return rand.N(d) }
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = DefaultLockTTL
	}
	w := &WorkItemController{
		s: s, cat: cat, conns: conns, factory: factory, opts: opts,
		log:        opts.Logger.With("controller", "work-item"),
		projected:  map[string]string{},
		lastResync: map[string]time.Time{},
	}
	w.ctrl = NewController("work-item", w, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := w.ctrl.Watch(s, v1alpha1.KindWorkItem, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return w, nil
}

// Controller returns the underlying controller.
func (w *WorkItemController) Controller() *Controller { return w.ctrl }

// Run runs the controller until ctx is done.
func (w *WorkItemController) Run(ctx context.Context) error { return w.ctrl.Run(ctx) }

func linkKey(wi string, l v1alpha1.Link) string { return wi + "|" + cacheKey(l) }

func digestOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// forget drops what the controller remembers of a work item, its stored baselines included.
// A delete that fails is logged: the leader's sweep deletes the orphan.
func (w *WorkItemController) forget(ctx context.Context, id string) {
	w.mu.Lock()
	delete(w.lastResync, id)
	for k := range w.projected {
		if strings.HasPrefix(k, id+"|") {
			delete(w.projected, k)
		}
	}
	w.mu.Unlock()
	if err := w.s.Delete(ctx, observationsKey(id)); err != nil && !errors.Is(err, store.ErrNotFound) {
		w.log.WarnContext(ctx, "delete a work item's resync baselines", "workItem", id, "err", err)
	}
}

// Reconcile implements Reconciler for one work item.
func (w *WorkItemController) Reconcile(ctx context.Context, id string) (Result, error) {
	var wi v1alpha1.WorkItem
	err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi)
	if errors.Is(err, store.ErrNotFound) {
		w.forget(ctx, id)
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get work item %s: %w", id, err)
	}
	if err := w.project(ctx, &wi); err != nil {
		return Result{}, err
	}
	interval := w.resyncInterval(&wi)
	now := w.opts.Clock.Now()
	w.mu.Lock()
	last, seen := w.lastResync[id]
	w.mu.Unlock()
	if !seen {
		// The first resync in this process. When a leader before this one stored baselines,
		// it isn't urgent: spread it over the interval rather than observing every item now.
		switch err := w.s.Get(ctx, observationsKey(id), &LinkObservations{}); {
		case err == nil:
			if delay := w.opts.Jitter(interval); delay > 0 {
				w.mu.Lock()
				w.lastResync[id] = now.Add(delay - interval)
				w.mu.Unlock()
				return Result{RequeueAfter: delay}, nil
			}
		case !errors.Is(err, store.ErrNotFound):
			return Result{}, fmt.Errorf("get the resync baselines of %s: %w", id, err)
		}
	}
	if seen && now.Before(last.Add(interval)) {
		return Result{RequeueAfter: last.Add(interval).Sub(now)}, nil
	}
	deleted, err := w.resync(ctx, &wi, now)
	if err != nil || deleted {
		return Result{}, err
	}
	w.mu.Lock()
	w.lastResync[id] = now
	w.mu.Unlock()
	return Result{RequeueAfter: interval}, nil
}

// projection is what the work item writes onto its linked objects.
func projection(wi *v1alpha1.WorkItem) provider.Projection {
	p := provider.Projection{WorkItemID: wi.Metadata.Name, Links: slices.Clone(wi.Spec.Links)}
	if st := wi.Status; st != nil {
		if len(st.State) > 0 {
			p.State, _ = jsonValue(st.State).(map[string]any)
		}
		if len(st.Counters) > 0 {
			p.Counters = maps.Clone(st.Counters)
		}
	}
	return p
}

// project writes the projection onto every link whose provider is connected, when it changed
// since the last write. The provider calls run under the work item's lock, on the item read
// again there, so an overlapping leader (P3 §Several dispatchers) can't write an older
// projection over a newer one. An object that no longer exists is skipped.
func (w *WorkItemController) project(ctx context.Context, wi *v1alpha1.WorkItem) error {
	if !w.projectionChanged(wi) {
		return nil
	}
	return withItemLock(ctx, w.s, wi.Metadata.Name, w.opts.LockTTL, func(ctx context.Context) error {
		var cur v1alpha1.WorkItem
		switch err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wi.Metadata.Name}, &cur); {
		case errors.Is(err, store.ErrNotFound):
			return nil // deleted meanwhile
		case err != nil:
			return fmt.Errorf("get work item %s: %w", wi.Metadata.Name, err)
		}
		return w.projectLocked(ctx, &cur)
	})
}

// projectionChanged reports whether a connected link's last projection differs from wi's.
func (w *WorkItemController) projectionChanged(wi *v1alpha1.WorkItem) bool {
	d := digestOf(projection(wi))
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range wi.Spec.Links {
		if l.Provider == v1alpha1.FactoryProvider {
			continue
		}
		if _, ok := w.conns.Conn(l.Provider); ok && w.projected[linkKey(wi.Metadata.Name, l)] != d {
			return true
		}
	}
	return false
}

// projectLocked is project's writes, with the work item's lock held.
func (w *WorkItemController) projectLocked(ctx context.Context, wi *v1alpha1.WorkItem) error {
	p := projection(wi)
	d := digestOf(p)
	for _, l := range wi.Spec.Links {
		if l.Provider == v1alpha1.FactoryProvider {
			continue
		}
		conn, ok := w.conns.Conn(l.Provider)
		if !ok {
			continue
		}
		k := linkKey(wi.Metadata.Name, l)
		w.mu.Lock()
		same := w.projected[k] == d
		w.mu.Unlock()
		if same {
			continue
		}
		err := conn.Project(ctx, v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref}, p)
		switch {
		case isNotFound(err):
			w.log.InfoContext(ctx, "projection skipped: object gone", "workItem", wi.Metadata.Name, "ref", l.Ref)
		case err != nil:
			return fmt.Errorf("project %s onto %s: %w", wi.Metadata.Name, l.Ref, err)
		}
		w.mu.Lock()
		w.projected[k] = d
		w.mu.Unlock()
	}
	return nil
}

// resyncInterval is the shortest resync.every of the item's providers, else the default.
func (w *WorkItemController) resyncInterval(wi *v1alpha1.WorkItem) time.Duration {
	interval := time.Duration(0)
	if cat := w.cat.Current(); cat != nil {
		for _, l := range wi.Spec.Links {
			p := cat.Providers[l.Provider]
			if p == nil || p.Spec.Resync == nil {
				continue
			}
			if d, err := p.Spec.Resync.Every.Parse(); err == nil && d > 0 && (interval == 0 || d < interval) {
				interval = d
			}
		}
	}
	if interval == 0 {
		interval = w.opts.ResyncInterval
	}
	return interval
}

// observationDigest digests what an out-of-band change can move: not the timestamp, nor the
// projections the factory itself writes. Observe reports those until P3's PR 6 removes
// Observation.Projections; after that the "projections" key is never there and its deletion
// is a no-op. It is deleted by its JSON name, not the Go field, so that removal doesn't touch
// this file. No deployed baseline depends on the encoding: LinkObservations is new in the
// same release.
func observationDigest(o provider.Observation) string {
	o.UpdatedAt = time.Time{}
	b, err := json.Marshal(o)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	delete(m, "projections")
	return digestOf(m)
}

// resync re-observes the item's links. When an observation changed since the baseline stored
// in its LinkObservations it emits factory.workitem.resynced, then stores the new baselines
// (only when they changed); when every link is closed and the item has been idle past the
// retention it deletes the item (true).
func (w *WorkItemController) resync(ctx context.Context, wi *v1alpha1.WorkItem, now time.Time) (bool, error) {
	var base LinkObservations
	stored := true
	switch err := w.s.Get(ctx, observationsKey(wi.Metadata.Name), &base); {
	case errors.Is(err, store.ErrNotFound):
		stored = false
	case err != nil:
		return false, fmt.Errorf("get the resync baselines of %s: %w", wi.Metadata.Name, err)
	}
	var changed []string
	open := false
	digests := map[string]string{}
	for _, l := range wi.Spec.Links {
		k := cacheKey(l)
		if _, ok := w.conns.Conn(l.Provider); !ok {
			open = true // can't tell: keep the item, and its baseline
			if d, ok := base.Digests[k]; ok {
				digests[k] = d
			}
			continue
		}
		obs, err := w.opts.Cache.Refresh(ctx, l)
		switch {
		case isNotFound(err):
			digests[k] = "gone"
		case err != nil:
			return false, fmt.Errorf("resync %s: %w", l.Ref, err)
		default:
			digests[k] = observationDigest(obs)
			if obs.State == provider.StateOpen {
				open = true
			}
		}
		if prev, ok := base.Digests[k]; ok && prev != digests[k] {
			changed = append(changed, l.Provider+"/"+l.Ref)
		}
	}
	if len(changed) > 0 {
		if _, err := w.factory.Emit(ctx, FactoryEvent{
			Type: v1alpha1.EventFactoryWorkItemResynced, WorkItem: wi.Metadata.Name,
			Key:   "resync/" + wi.Metadata.Name + "/" + digestOf(digests)[:16],
			Facts: map[string]any{"resync": map[string]any{"changed": jsonValue(changed)}},
		}); err != nil {
			return false, err
		}
		w.log.InfoContext(ctx, "work item resynced", "workItem", wi.Metadata.Name, "changed", changed)
	}
	if err := w.storeBaselines(ctx, wi.Metadata.Name, &base, stored, digests); err != nil {
		return false, err
	}
	if open || len(wi.Spec.Links) == 0 {
		return false, nil
	}
	last, err := w.lastActivity(ctx, wi)
	if err != nil {
		return false, err
	}
	if now.Sub(last) < w.opts.Retention {
		return false, nil
	}
	return w.retire(ctx, wi.Metadata.Name, last)
}

// storeBaselines writes digests as the item's LinkObservations when they differ from base (a
// digest changed, a link was added or removed): a create, or a compare-and-swap on the
// version read. A conflict (another leader wrote meanwhile) retries the reconcile; the
// resynced event it may emit again has the same key, so it isn't stored twice.
func (w *WorkItemController) storeBaselines(ctx context.Context, id string, base *LinkObservations, stored bool, digests map[string]string) error {
	if stored && maps.Equal(base.Digests, digests) {
		return nil
	}
	if !stored && len(digests) == 0 {
		return nil
	}
	var err error
	if stored {
		base.Digests = digests
		err = w.s.Update(ctx, base)
	} else {
		err = w.s.Create(ctx, &LinkObservations{
			TypeMeta: v1alpha1.TypeMeta{Kind: KindLinkObservations},
			Metadata: v1alpha1.ObjectMeta{Name: id},
			Digests:  digests,
		})
	}
	if err != nil {
		return fmt.Errorf("store the resync baselines of %s: %w", id, err)
	}
	return nil
}

// SweepObservations deletes the LinkObservations whose work item no longer exists (a delete
// whose baselines' delete failed), and returns how many it deleted. The leader's sweep runs it.
func (w *WorkItemController) SweepObservations(ctx context.Context) (int, error) {
	var recs []*LinkObservations
	if _, err := w.s.List(ctx, KindLinkObservations, store.ListOptions{}, &recs); err != nil {
		return 0, fmt.Errorf("list resync baselines: %w", err)
	}
	n := 0
	for _, r := range recs {
		switch err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: r.Metadata.Name}, &v1alpha1.WorkItem{}); {
		case err == nil:
			continue
		case !errors.Is(err, store.ErrNotFound):
			return n, fmt.Errorf("get work item %s: %w", r.Metadata.Name, err)
		}
		switch err := w.s.Delete(ctx, store.KeyOf(r)); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return n, fmt.Errorf("delete the resync baselines of %s: %w", r.Metadata.Name, err)
		default:
			n++
		}
	}
	return n, nil
}

// lastActivity is the latest of the item's creation, decisions, tasks and runs.
func (w *WorkItemController) lastActivity(ctx context.Context, wi *v1alpha1.WorkItem) (time.Time, error) {
	last := wi.Metadata.CreatedAt
	if st := wi.Status; st != nil {
		for _, d := range st.Decisions {
			if d.At.After(last) {
				last = d.At
			}
		}
	}
	tasks, err := listTasks(ctx, w.s, wi.Metadata.Name)
	if err != nil {
		return time.Time{}, err
	}
	for _, t := range tasks {
		if t.Metadata.CreatedAt.After(last) {
			last = t.Metadata.CreatedAt
		}
		if activePhase(taskPhase(t)) {
			return w.opts.Clock.Now(), nil // still working on it
		}
		runs, err := listRuns(ctx, w.s, t.Metadata.ID)
		if err != nil {
			return time.Time{}, err
		}
		for _, r := range runs {
			if r.Status != nil && r.Status.EndedAt.After(last) {
				last = r.Status.EndedAt
			}
		}
	}
	return last, nil
}

// retire deletes an idle, closed work item under its lock, re-checking that nothing happened
// to it in between.
func (w *WorkItemController) retire(ctx context.Context, id string, last time.Time) (bool, error) {
	var retired bool
	err := withItemLock(ctx, w.s, id, w.opts.LockTTL, func(ctx context.Context) error {
		var err error
		retired, err = w.retireLocked(ctx, id, last)
		return err
	})
	return retired, err
}

// retireLocked is retire with the work item's lock held.
func (w *WorkItemController) retireLocked(ctx context.Context, id string, last time.Time) (bool, error) {
	var wi v1alpha1.WorkItem
	switch err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("get work item %s: %w", id, err)
	}
	if again, err := w.lastActivity(ctx, &wi); err != nil || again.After(last) {
		return false, err
	}
	if err := w.s.Delete(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("delete work item %s: %w", id, err)
	}
	w.forget(ctx, id)
	w.log.InfoContext(ctx, "work item retired", "workItem", id, "lastActivity", last)
	return true, nil
}
