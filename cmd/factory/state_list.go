package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store/filestore"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// ---- tasks -------------------------------------------------------------------------------------

func stateTasks(ctx context.Context, r *stateRun, args []string) int {
	fl := r.flags("[--phase P] [--spec S] [--workitem W] [--since 24h]", true)
	phase := fl.String("phase", "", "only tasks in this phase ("+strings.Join(phaseNames(), ", ")+")")
	spec := fl.String("spec", "", "only tasks of this TaskSpec")
	workItem := fl.String("workitem", "", "only the tasks of this work item")
	sinceFlag := fl.String("since", "", "only tasks created this long ago or less (30m, 24h, 7d)")
	if _, code, ok := r.parse(fl, args, 0); !ok {
		return code
	}
	if *phase != "" {
		i := slices.IndexFunc(phaseNames(), func(p string) bool { return strings.EqualFold(p, *phase) })
		if i < 0 {
			fmt.Fprintf(r.stderr, "%s: --phase %q: want one of %s\n", r.name, *phase, strings.Join(phaseNames(), ", "))
			return exitUsage
		}
		*phase = phaseNames()[i]
	}
	since, err := r.since(*sinceFlag)
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
		return exitUsage
	}
	if code, ok := r.open(); !ok {
		return code
	}
	tasks, err := listAll[v1alpha1.Task](ctx, r.view, v1alpha1.KindTask)
	if err != nil {
		return r.fail(err)
	}
	tasks = slices.DeleteFunc(tasks, func(t v1alpha1.Task) bool {
		return (*phase != "" && string(taskPhase(&t)) != *phase) ||
			(*spec != "" && t.Spec.TaskSpec.Name != *spec) ||
			(*workItem != "" && t.Metadata.WorkItem != *workItem) ||
			(!since.IsZero() && t.Metadata.CreatedAt.Before(since))
	})
	sortTasks(tasks)
	if r.json {
		return r.writeJSON(nonNil(tasks))
	}
	runs, err := runsByTask(ctx, r.view)
	if err != nil {
		return r.fail(err)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(r.stdout, "no tasks")
		return 0
	}
	taskTable(r.stdout, tasks, runs, true)
	return 0
}

// nonNil is s, or an empty slice for nil, so --json prints [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func phaseNames() []string {
	var out []string
	for _, p := range v1alpha1.TaskPhases() {
		out = append(out, string(p))
	}
	return out
}

func taskPhase(t *v1alpha1.Task) v1alpha1.TaskPhase {
	if t.Status == nil {
		return ""
	}
	return t.Status.Phase
}

// sortTasks orders tasks by creation, then id.
func sortTasks(tasks []v1alpha1.Task) {
	slices.SortFunc(tasks, func(a, b v1alpha1.Task) int {
		return cmp.Or(a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt), cmp.Compare(a.Metadata.ID, b.Metadata.ID))
	})
}

// runsByTask returns every run by task, in attempt order.
func runsByTask(ctx context.Context, v *filestore.View) (map[string][]v1alpha1.Run, error) {
	runs, err := listAll[v1alpha1.Run](ctx, v, v1alpha1.KindRun)
	if err != nil {
		return nil, err
	}
	out := map[string][]v1alpha1.Run{}
	for _, run := range runs {
		out[run.Spec.Task] = append(out[run.Spec.Task], run)
	}
	for _, rs := range out {
		sortRuns(rs)
	}
	return out, nil
}

func sortRuns(runs []v1alpha1.Run) {
	slices.SortFunc(runs, func(a, b v1alpha1.Run) int {
		return cmp.Or(cmp.Compare(a.Spec.Attempt, b.Spec.Attempt), cmp.Compare(a.Metadata.Name, b.Metadata.Name))
	})
}

// taskTable prints tasks with their last run; withWorkItem adds the work item column.
func taskTable(w io.Writer, tasks []v1alpha1.Task, runs map[string][]v1alpha1.Run, withWorkItem bool) {
	tw := newTable(w)
	head := []string{"ID", "SPEC", "PHASE", "REASON"}
	if withWorkItem {
		head = append(head, "WORK ITEM")
	}
	row(tw, append(head, "CREATED", "LAST RUN")...)
	for _, t := range tasks {
		reason := ""
		if t.Status != nil {
			reason = t.Status.Reason
		}
		cells := []string{t.Metadata.ID, t.Spec.TaskSpec.Name, string(taskPhase(&t)), reason}
		if withWorkItem {
			cells = append(cells, t.Metadata.WorkItem)
		}
		last := ""
		if rs := runs[t.Metadata.ID]; len(rs) > 0 {
			last = rs[len(rs)-1].Metadata.Name + " " + runOutcome(&rs[len(rs)-1])
		}
		row(tw, append(cells, ts(t.Metadata.CreatedAt), last)...)
	}
	_ = tw.Flush()
}

// runOutcome is how a run ended, with its Result's status: "running", "lost",
// "completed: failed".
func runOutcome(run *v1alpha1.Run) string {
	st := run.Status
	switch {
	case st == nil || st.End == "":
		return "running"
	case st.Result != nil:
		return string(st.End) + ": " + string(st.Result.Status)
	default:
		return string(st.End)
	}
}

// ---- events ------------------------------------------------------------------------------------

func stateEvents(ctx context.Context, r *stateRun, args []string) int {
	fl := r.flags("[--since 1h] [--type T] [--workitem W]", true)
	sinceFlag := fl.String("since", "", "only events this recent (by their time: 30m, 24h, 7d)")
	typ := fl.String("type", "", "only events of this type (issue.opened, pr.merged, factory.run.completed…)")
	workItem := fl.String("workitem", "", "only the events of this work item")
	if _, code, ok := r.parse(fl, args, 0); !ok {
		return code
	}
	since, err := r.since(*sinceFlag)
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
		return exitUsage
	}
	if code, ok := r.open(); !ok {
		return code
	}
	events, err := listAll[v1alpha1.Event](ctx, r.view, v1alpha1.KindEvent)
	if err != nil {
		return r.fail(err)
	}
	statuses, err := listAll[controllers.EventStatus](ctx, r.view, controllers.KindEventStatus)
	if err != nil {
		return r.fail(err)
	}
	processed := map[string]controllers.EventStatus{}
	for _, s := range statuses {
		processed[s.Metadata.Name] = s
	}
	itemOf := func(ev *v1alpha1.Event) string {
		return cmp.Or(ev.WorkItem, processed[ev.ID].WorkItem)
	}
	events = slices.DeleteFunc(events, func(ev v1alpha1.Event) bool {
		return (!since.IsZero() && ev.Time.Before(since)) ||
			(*typ != "" && string(ev.Type) != *typ) ||
			(*workItem != "" && itemOf(&ev) != *workItem)
	})
	slices.SortFunc(events, func(a, b v1alpha1.Event) int {
		return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(a.ID, b.ID))
	})
	if r.json {
		return r.writeJSON(nonNil(events))
	}
	if len(events) == 0 {
		fmt.Fprintln(r.stdout, "no events")
		return 0
	}
	tw := newTable(r.stdout)
	row(tw, "TIME", "ID", "TYPE", "SOURCE", "OBJECT", "WORK ITEM", "PROCESSED")
	for _, ev := range events {
		obj := ev.Subject
		if o := ev.Data.Object; o != nil {
			obj = strings.TrimSpace(o.Kind + " " + o.Ref)
		}
		done := "pending"
		if s, ok := processed[ev.ID]; ok {
			done = ts(s.ProcessedAt)
			if s.WorkItem == "" {
				done += " (no rule matched)"
			}
		}
		row(tw, ts(ev.Time), ev.ID, string(ev.Type), ev.Source, obj, itemOf(&ev), done)
	}
	_ = tw.Flush()
	return 0
}

// ---- log ---------------------------------------------------------------------------------------

// logLine is a journal line as --json prints it: the shape of journal.jsonl.
type logLine struct {
	V    string          `json:"v"`
	At   time.Time       `json:"at"`
	Op   memory.Op       `json:"op"`
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Obj  json.RawMessage `json:"obj,omitempty"`
}

func stateLog(_ context.Context, r *stateRun, args []string) int {
	fl := r.flags("[--since 10m]", true)
	sinceFlag := fl.String("since", "", "only changes this recent (10m, 2h, 1d)")
	if _, code, ok := r.parse(fl, args, 0); !ok {
		return code
	}
	since, err := r.since(*sinceFlag)
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
		return exitUsage
	}
	if code, ok := r.open(); !ok {
		return code
	}
	journal := r.view.Journal()
	var n noter
	notes := make([]string, len(journal))
	for i, e := range journal {
		notes[i] = n.note(e)
	}
	keep := func(e filestore.JournalEntry) bool { return since.IsZero() || !e.At.Before(since) }

	if r.json {
		out := []logLine{}
		for _, e := range journal {
			if keep(e) {
				out = append(out, logLine{V: fmt.Sprint(e.Version), At: e.At, Op: e.Op, Kind: e.Kind, Name: e.Name, Obj: e.Obj})
			}
		}
		return r.writeJSON(out)
	}
	li := r.view.Loaded()
	if len(journal) == 0 {
		fmt.Fprintf(r.stdout, "the journal is empty: every change up to version %d is in the snapshot\n", li.Version)
		return 0
	}
	fmt.Fprintf(r.stdout, "%d changes since the snapshot (versions %d to %d); older ones are folded into the snapshot\n",
		len(journal), journal[0].Version, li.Version)
	tw := newTable(r.stdout)
	row(tw, "VERSION", "TIME", "OP", "KIND", "NAME", "NOTE")
	for i, e := range journal {
		if keep(e) {
			row(tw, fmt.Sprint(e.Version), ts(e.At), string(e.Op), e.Kind, e.Name, notes[i])
		}
	}
	_ = tw.Flush()
	return 0
}

// noter makes the log's highlights: Task phase changes and Run starts and ends. It follows
// each object through the journal; an object's first line has no previous state to compare
// with (it is in the snapshot).
type noter struct {
	phases map[string]v1alpha1.TaskPhase
	ends   map[string]v1alpha1.RunEnd
}

func (n *noter) note(e filestore.JournalEntry) string {
	if n.phases == nil {
		n.phases, n.ends = map[string]v1alpha1.TaskPhase{}, map[string]v1alpha1.RunEnd{}
	}
	switch e.Kind {
	case v1alpha1.KindTask:
		if e.Op == memory.OpDelete {
			delete(n.phases, e.Name)
			return ""
		}
		var t v1alpha1.Task
		if json.Unmarshal(e.Obj, &t) != nil || t.Status == nil {
			return ""
		}
		prev, seen := n.phases[e.Name]
		n.phases[e.Name] = t.Status.Phase
		if seen && prev == t.Status.Phase {
			return ""
		}
		note := "phase " + string(t.Status.Phase)
		if seen {
			note = "phase " + string(prev) + " → " + string(t.Status.Phase)
		}
		if t.Status.Reason != "" && (t.Status.Phase == v1alpha1.TaskFailed || t.Status.Phase == v1alpha1.TaskCancelled || t.Status.Phase == v1alpha1.TaskPending) {
			note += ": " + t.Status.Reason
		}
		return note
	case v1alpha1.KindRun:
		if e.Op == memory.OpDelete {
			delete(n.ends, e.Name)
			return ""
		}
		var run v1alpha1.Run
		if json.Unmarshal(e.Obj, &run) != nil {
			return ""
		}
		end := v1alpha1.RunEnd("")
		if run.Status != nil {
			end = run.Status.End
		}
		prev, seen := n.ends[e.Name]
		n.ends[e.Name] = end
		switch {
		case e.Op == memory.OpCreate:
			return fmt.Sprintf("attempt %d of %s on %s", run.Spec.Attempt, run.Spec.Task, run.Spec.Agent)
		case end == "" || (seen && prev == end):
			return ""
		}
		note := "ended " + runOutcome(&run)
		if res := run.Status.Result; res != nil && res.Outcome != "" {
			note += " (" + res.Outcome + ")"
		}
		return note
	}
	return ""
}
