package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

// ErrTaskLeased is DeleteWorkItem's refusal, without force, while one of the item's tasks is
// leased (Leased, Running or Cancelling).
var ErrTaskLeased = errors.New("a task of the work item is leased")

// maxAdminCAS bounds the compare-and-swap retries of CloseWorkItem and the task cancels.
const maxAdminCAS = 10

// Closed reports whether an admin closed the work item (status.closed).
func Closed(wi *v1alpha1.WorkItem) bool { return wi.Status != nil && wi.Status.Closed != nil }

// closedRecord is the decision recorded for an event of a closed work item: no rule is
// evaluated for it.
func closedRecord(wi *v1alpha1.WorkItem, event string, at time.Time) v1alpha1.Decision {
	return v1alpha1.Decision{At: at, Event: event,
		Reason: fmt.Sprintf("the work item is closed (%s): no rules evaluated", wi.Status.Closed.Reason)}
}

// CloseWorkItem closes the work item id under its lock: it sets status.closed, appends a
// decision saying who closed it and why, and moves the item's active tasks (Pending, Leased,
// Running) to Cancelling, as the cancel command does (the run controller cancels a task no
// agent holds at once; an agent learns it from its next heartbeat). Closing a closed item
// updates its reason and note. It returns the item and the tasks it cancelled; a missing
// item is store.ErrNotFound.
func CloseWorkItem(ctx context.Context, s store.Store, id string, closed v1alpha1.WorkItemClosed) (*v1alpha1.WorkItem, []string, error) {
	if !closed.Reason.Valid() {
		return nil, nil, fmt.Errorf("close work item %s: reason %q: %w", id, closed.Reason, store.ErrInvalid)
	}
	var (
		wi        *v1alpha1.WorkItem
		cancelled []string
	)
	err := withItemLock(ctx, s, id, DefaultLockTTL, func(ctx context.Context) error {
		var err error
		if wi, err = closeLocked(ctx, s, id, closed); err != nil {
			return err
		}
		cancelled, err = cancelActive(ctx, s, id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return wi, cancelled, nil
}

func closeLocked(ctx context.Context, s store.Store, id string, closed v1alpha1.WorkItemClosed) (*v1alpha1.WorkItem, error) {
	reason := "closed as " + string(closed.Reason)
	if closed.By != "" {
		reason += " by " + closed.By
	}
	if closed.Note != "" {
		reason += ": " + closed.Note
	}
	for range maxAdminCAS {
		var wi v1alpha1.WorkItem
		if err := s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); err != nil {
			return nil, fmt.Errorf("get work item %s: %w", id, err)
		}
		c := closed
		ensureStatus(&wi).Closed = &c
		appendDecisions(&wi, []v1alpha1.Decision{{At: closed.At, Reason: reason}})
		err := s.Update(ctx, &wi)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("close work item %s: %w", id, err)
		}
		return &wi, nil
	}
	return nil, fmt.Errorf("close work item %s: %w", id, store.ErrConflict)
}

// cancelActive moves the work item's Pending, Leased and Running tasks to Cancelling and
// returns their ids.
func cancelActive(ctx context.Context, s store.Store, id string) ([]string, error) {
	tasks, err := listTasks(ctx, s, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tasks {
		ok, err := cancelTask(ctx, s, t.Metadata.ID)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, t.Metadata.ID)
		}
	}
	return out, nil
}

// cancelTask moves one task to Cancelling when it is Pending, Leased or Running, and reports
// whether it did.
func cancelTask(ctx context.Context, s store.Store, id string) (bool, error) {
	for range maxAdminCAS {
		var t v1alpha1.Task
		switch err := s.Get(ctx, store.Key{Kind: v1alpha1.KindTask, Name: id}, &t); {
		case errors.Is(err, store.ErrNotFound):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("get task %s: %w", id, err)
		}
		switch taskPhase(&t) {
		case v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning:
		default:
			return false, nil
		}
		if t.Status == nil {
			t.Status = &v1alpha1.TaskStatus{}
		}
		t.Status.Phase, t.Status.Reason = v1alpha1.TaskCancelling, ReasonCancelled
		err := s.Update(ctx, &t)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("cancel task %s: %w", id, err)
		}
		return true, nil
	}
	return false, fmt.Errorf("cancel task %s: %w", id, store.ErrConflict)
}

// Deleted is what DeleteWorkItem removed: how many objects of each kind, and the ids of the
// item's tasks (so the caller can drop what it keys to them, the scheduler's leases).
type Deleted struct {
	Kinds map[string]int
	Tasks []string
}

// DeleteWorkItem deletes the work item id, under its lock, with everything keyed to it: its
// tasks and their prompts, runs (the store drops their transcripts) and effects; its events
// (the ones processed for it, the ones its decisions name and the factory events about it),
// their processed marks and the rule effects they made; its debounces. Without force it
// refuses with ErrTaskLeased while a task is Leased, Running or Cancelling; with force it
// cancels the active tasks first. A missing item is store.ErrNotFound.
func DeleteWorkItem(ctx context.Context, s store.Store, id string, force bool) (Deleted, error) {
	var out Deleted
	err := withItemLock(ctx, s, id, DefaultLockTTL, func(ctx context.Context) error {
		var err error
		out, err = deleteLocked(ctx, s, id, force)
		return err
	})
	return out, err
}

func deleteLocked(ctx context.Context, s store.Store, id string, force bool) (Deleted, error) {
	out := Deleted{Kinds: map[string]int{}}
	var wi v1alpha1.WorkItem
	if err := s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); err != nil {
		return out, fmt.Errorf("get work item %s: %w", id, err)
	}
	tasks, err := listTasks(ctx, s, id)
	if err != nil {
		return out, err
	}
	for _, t := range tasks {
		switch taskPhase(t) {
		case v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
			if !force {
				return out, fmt.Errorf("delete work item %s: task %s is %s: %w", id, t.Metadata.ID, taskPhase(t), ErrTaskLeased)
			}
		}
	}
	if force {
		if _, err := cancelActive(ctx, s, id); err != nil {
			return out, err
		}
	}
	del := func(kind, name string) error {
		err := s.Delete(ctx, store.Key{Kind: kind, Name: name})
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil
		case err != nil:
			return fmt.Errorf("delete %s %s: %w", kind, name, err)
		}
		out.Kinds[kind]++
		return nil
	}

	// The item's events: processed for it, named by its decisions, or factory events about it.
	events := map[string]bool{}
	var marks []*EventStatus
	if _, err := s.List(ctx, KindEventStatus, store.ListOptions{}, &marks); err != nil {
		return out, fmt.Errorf("list event marks: %w", err)
	}
	for _, m := range marks {
		if m.WorkItem == id {
			events[m.Metadata.Name] = true
		}
	}
	if wi.Status != nil {
		for _, d := range wi.Status.Decisions {
			if d.Event != "" {
				events[d.Event] = true
			}
		}
	}
	var all []*v1alpha1.Event
	if _, err := s.List(ctx, v1alpha1.KindEvent, store.ListOptions{}, &all); err != nil {
		return out, fmt.Errorf("list events: %w", err)
	}
	known := map[string]bool{}
	for _, ev := range all {
		known[ev.ID] = true
		if ev.WorkItem == id {
			events[ev.ID] = true
		}
	}
	for ev := range events {
		known[ev] = true
	}
	var debounces []*DebounceRecord
	if _, err := s.List(ctx, KindDebounce, store.ListOptions{}, &debounces); err != nil {
		return out, fmt.Errorf("list debounces: %w", err)
	}
	var mine []string
	for _, d := range debounces {
		if d.Spec.WorkItem == id {
			mine = append(mine, d.Metadata.Name)
		}
	}

	// Effects: the tasks', and the rule effects of its events and debounces (by their keys).
	taskSet := map[string]bool{}
	for _, t := range tasks {
		taskSet[t.Metadata.ID] = true
		out.Tasks = append(out.Tasks, t.Metadata.ID)
	}
	var effects []*v1alpha1.Effect
	if _, err := s.List(ctx, v1alpha1.KindEffect, store.ListOptions{}, &effects); err != nil {
		return out, fmt.Errorf("list effects: %w", err)
	}
	for _, e := range effects {
		if taskSet[e.Spec.Task] || ruleEffectOf(e.Spec.IdempotencyKey, known, events, mine) {
			if err := del(v1alpha1.KindEffect, e.Metadata.Name); err != nil {
				return out, err
			}
		}
	}
	for _, t := range tasks {
		runs, err := listRuns(ctx, s, t.Metadata.ID)
		if err != nil {
			return out, err
		}
		for _, r := range runs {
			if err := del(v1alpha1.KindRun, r.Metadata.Name); err != nil {
				return out, err
			}
		}
		if err := del(KindTaskPrompt, t.Metadata.ID); err != nil {
			return out, err
		}
		if err := del(v1alpha1.KindTask, t.Metadata.ID); err != nil {
			return out, err
		}
	}
	for _, name := range mine {
		if err := del(KindDebounce, name); err != nil {
			return out, err
		}
	}
	for ev := range events {
		if err := del(KindEventStatus, ev); err != nil {
			return out, err
		}
		if err := del(v1alpha1.KindEvent, ev); err != nil {
			return out, err
		}
	}
	if err := del(v1alpha1.KindWorkItem, id); err != nil {
		return out, err
	}
	return out, nil
}

// ruleEffectOf reports whether an Effect's idempotency key is a rule effect of one of the
// item's events ("evt/<event>/<rule>/<i>…") or debounces ("dbn/<name>/<i>"). Event ids and
// inline rule names hold "/", so the key's event is the longest known event id it starts
// with: "evt/p/d/0/r/0" is event p/d/0's when both p/d and p/d/0 exist.
func ruleEffectOf(key string, known, events map[string]bool, debounces []string) bool {
	if rest, ok := strings.CutPrefix(key, "evt/"); ok {
		ev := ""
		for i := range len(rest) {
			if rest[i] == '/' && known[rest[:i]] {
				ev = rest[:i]
			}
		}
		return ev != "" && events[ev]
	}
	if rest, ok := strings.CutPrefix(key, "dbn/"); ok {
		for _, d := range debounces {
			if strings.HasPrefix(rest, d+"/") {
				return true
			}
		}
	}
	return false
}
