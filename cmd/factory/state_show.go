package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// get reads one object; found is false (and the not-found message printed, with the nearest
// ids) when there is none. code is the exit code when ok is false.
func (r *stateRun) get(ctx context.Context, kind, id string, out store.Object) (code int, ok bool) {
	err := r.view.Get(ctx, store.Key{Kind: kind, Name: id}, out)
	if errors.Is(err, store.ErrNotFound) {
		var all []store.Object
		if _, err := r.view.List(ctx, kind, store.ListOptions{}, &all); err != nil {
			return r.fail(err), false
		}
		ids := make([]string, len(all))
		for i, o := range all {
			ids[i] = o.GetName()
		}
		return r.notFound(kind, id, ids), false
	}
	if err != nil {
		return r.fail(err), false
	}
	return 0, true
}

// ---- task --------------------------------------------------------------------------------------

// taskView is factory state task --json.
type taskView struct {
	Task v1alpha1.Task  `json:"task"`
	Runs []v1alpha1.Run `json:"runs"`
	// Decision is the work item's decision that dispatched the task (its provenance), when the
	// work item still has it.
	Decision *v1alpha1.Decision `json:"decision,omitempty"`
}

func stateTask(ctx context.Context, r *stateRun, args []string) int {
	fl := r.flags("TASK", true)
	pos, code, ok := r.parse(fl, args, 1)
	if !ok {
		return code
	}
	if code, ok := r.open(); !ok {
		return code
	}
	var tv taskView
	if code, ok := r.get(ctx, v1alpha1.KindTask, pos[0], &tv.Task); !ok {
		return code
	}
	t := &tv.Task
	runs, err := listAll[v1alpha1.Run](ctx, r.view, v1alpha1.KindRun)
	if err != nil {
		return r.fail(err)
	}
	tv.Runs = slices.DeleteFunc(runs, func(run v1alpha1.Run) bool { return run.Spec.Task != t.Metadata.ID })
	sortRuns(tv.Runs)
	var wi v1alpha1.WorkItem
	wiErr := r.view.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: t.Metadata.WorkItem}, &wi)
	if wiErr != nil && !errors.Is(wiErr, store.ErrNotFound) {
		return r.fail(wiErr)
	}
	if p := t.Metadata.Provenance; p != nil && wiErr == nil && wi.Status != nil {
		for i := len(wi.Status.Decisions) - 1; i >= 0; i-- {
			d := wi.Status.Decisions[i]
			if d.Fired && d.Event == p.Event && d.RuleSet == p.RuleSet && d.Rule == p.Rule {
				tv.Decision = &d
				break
			}
		}
	}
	if r.json {
		tv.Runs = nonNil(tv.Runs)
		return r.writeJSON(tv)
	}

	w := r.stdout
	fmt.Fprintf(w, "Task %s\n", t.Metadata.ID)
	sp := t.Spec
	field(w, "spec", strings.TrimSpace(sp.TaskSpec.Name+" "+paren(sp.TaskSpec.Digest)))
	field(w, "work item", t.Metadata.WorkItem)
	field(w, "queue", strings.TrimSpace(sp.Queue+" "+paren(prefixed("pool ", sp.Pool))))
	if tg := sp.Target; tg != nil {
		field(w, "target", joinNonEmpty(" ", tg.Provider, tg.Repo, tg.Ref))
	}
	field(w, "harness", harnessLine(sp.Harness))
	field(w, "inputs", mapLines(sp.Inputs))
	field(w, "created", ts(t.Metadata.CreatedAt))
	if st := t.Status; st != nil {
		field(w, "phase", strings.TrimSpace(string(st.Phase)+" "+paren(prefixed("attempts ", nonZero(st.Attempts)))))
		field(w, "reason", st.Reason)
		if st.Agent != nil {
			field(w, "agent", *st.Agent)
		}
		if !st.LeaseExpiresAt.IsZero() {
			field(w, "lease until", ts(st.LeaseExpiresAt))
		}
	}
	if p := t.Metadata.Provenance; p != nil {
		field(w, "provenance", fmt.Sprintf("event %s, rule %s/%s, catalog %s", p.Event, p.RuleSet, p.Rule, p.CatalogSHA))
	}

	fmt.Fprintln(w, "\nDispatched by")
	switch {
	case tv.Decision != nil:
		decisionLines(w, tv.Decision)
	case t.Metadata.Provenance == nil:
		fmt.Fprintln(w, "  no provenance: not dispatched by a rule")
	case wiErr != nil:
		fmt.Fprintf(w, "  work item %s not found; the provenance above names the rule\n", t.Metadata.WorkItem)
	default:
		fmt.Fprintf(w, "  no matching decision on %s (it keeps the newest %d); the provenance above names the rule\n", t.Metadata.WorkItem, v1alpha1.MaxDecisions)
	}

	fmt.Fprintf(w, "\nRuns (%d)\n", len(tv.Runs))
	if len(tv.Runs) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for i := range tv.Runs {
		run := &tv.Runs[i]
		st := run.Status
		if st == nil {
			st = &v1alpha1.RunStatus{}
		}
		fmt.Fprintf(w, "  %s  attempt %d  %s  %s → %s  %s\n", run.Metadata.Name, run.Spec.Attempt, cell(run.Spec.Agent),
			ts(st.StartedAt), ts(st.EndedAt), runOutcome(run))
		if res := st.Result; res != nil {
			fmt.Fprintf(w, "    %-8s %s\n", "result", joinNonEmpty(": ", string(res.Status), res.Outcome))
			if res.Summary != "" {
				fmt.Fprintf(w, "    %-8s %s\n", "summary", multiline(res.Summary, strings.Repeat(" ", 13)))
			}
		}
	}
	return 0
}

// harnessLine is the requested harness and, once leased, the resolved one.
func harnessLine(h *v1alpha1.TaskHarness) string {
	if h == nil {
		return ""
	}
	var req, res string
	if q := h.Request; q != nil {
		parts := []string{q.Name}
		if c := q.Config; c != nil {
			parts = append(parts, prefixed("model ", strings.Join(c.Model.Values, "|")), prefixed("effort ", strings.Join(c.ReasoningEffort.Values, "|")))
		}
		req = joinNonEmpty(", ", parts...)
	}
	if s := h.Resolved; s != nil {
		res = joinNonEmpty(" ", s.Name, s.Version, s.Model, s.ReasoningEffort, paren(s.Auth))
	}
	switch {
	case res == "":
		return joinNonEmpty(" ", req, "(not resolved yet)")
	case req == "":
		return res
	}
	return req + " → " + res
}

// decisionLines prints one decision: when, which rule, on which event, the verdict, the
// actions and the rails.
func decisionLines(w io.Writer, d *v1alpha1.Decision) {
	fmt.Fprintf(w, "  %s  %s/%s on %s: %s\n", ts(d.At), d.RuleSet, d.Rule, d.Event, verdict(d))
	if len(d.Actions) > 0 {
		fmt.Fprintf(w, "    %-8s %s\n", "actions", cell(strings.Join(d.Actions, "; ")))
	}
	if len(d.Rails) > 0 {
		var rails []string
		for _, rv := range d.Rails {
			rails = append(rails, joinNonEmpty(": ", rv.Rail+" "+string(rv.Verdict), rv.Reason))
		}
		fmt.Fprintf(w, "    %-8s %s\n", "rails", multiline(strings.Join(rails, "\n"), strings.Repeat(" ", 13)))
	}
}

// verdict is what a decision did: "fired", "fired; budget deferred", "not fired: reason".
func verdict(d *v1alpha1.Decision) string {
	if !d.Fired {
		return joinNonEmpty(": ", "not fired", d.Reason)
	}
	var held []string
	for _, rv := range d.Rails {
		if rv.Verdict != v1alpha1.RailAllowed {
			held = append(held, rv.Rail+" "+string(rv.Verdict))
		}
	}
	return joinNonEmpty("; ", "fired", strings.Join(held, ", "))
}

// ---- run ---------------------------------------------------------------------------------------

func stateRunCmd(ctx context.Context, r *stateRun, args []string) int {
	fl := r.flags("RUN [--transcript]", true)
	transcript := fl.Bool("transcript", false, "print the run's transcript (JSON Lines, gunzipped) and nothing else")
	pos, code, ok := r.parse(fl, args, 1)
	if !ok {
		return code
	}
	if code, ok := r.open(); !ok {
		return code
	}
	var run v1alpha1.Run
	if code, ok := r.get(ctx, v1alpha1.KindRun, pos[0], &run); !ok {
		return code
	}
	tr, trErr := r.view.Transcript(run.Metadata.Name)
	if trErr != nil && !errors.Is(trErr, store.ErrNotFound) {
		return r.fail(trErr)
	}
	if *transcript {
		if trErr != nil {
			fmt.Fprintf(r.stderr, "%s: run %s has no transcript in %s\n", r.name, run.Metadata.Name, r.view.Dir())
			return 1
		}
		zr, err := gzip.NewReader(bytes.NewReader(tr))
		if err != nil {
			return r.fail(fmt.Errorf("the transcript of %s isn't gzip'd: %w", run.Metadata.Name, err))
		}
		if _, err := io.Copy(r.stdout, zr); err != nil { //nolint:gosec // the agent caps transcripts at upload (20 MB)
			return r.fail(fmt.Errorf("gunzip the transcript of %s: %w", run.Metadata.Name, err))
		}
		return 0
	}
	if r.json {
		return r.writeJSON(run)
	}

	w := r.stdout
	st := run.Status
	if st == nil {
		st = &v1alpha1.RunStatus{}
	}
	fmt.Fprintf(w, "Run %s\n", run.Metadata.Name)
	field(w, "task", fmt.Sprintf("%s (attempt %d)", run.Spec.Task, run.Spec.Attempt))
	field(w, "agent", strings.TrimSpace(run.Spec.Agent+" "+paren(prefixed("owner ", run.Spec.Owner))))
	field(w, "started", ts(st.StartedAt))
	field(w, "ended", strings.TrimSpace(ts(st.EndedAt)+" "+paren(runOutcome(&run))))
	if trErr == nil {
		field(w, "transcript", fmt.Sprintf("%d bytes gzip'd: factory state run %s --transcript", len(tr), run.Metadata.Name))
	} else {
		field(w, "transcript", "none")
	}

	fmt.Fprintln(w, "\nResult")
	if res := st.Result; res == nil {
		fmt.Fprintln(w, "  none reported")
	} else {
		resultLines(w, res)
	}

	fmt.Fprintln(w, "\nUsage")
	usage := st.Usage
	if usage == nil && st.Result != nil && st.Result.Harness != nil {
		usage = &st.Result.Harness.Usage
	}
	if usage == nil {
		fmt.Fprintln(w, "  none reported")
	} else {
		field(w, "tokens", fmt.Sprintf("%d in, %d out", usage.InputTokens, usage.OutputTokens))
		field(w, "turns", nonZero(usage.Turns))
		field(w, "wall clock", string(usage.WallClock))
		cost := "none (a subscription run)"
		if usage.CostUSD != nil {
			cost = fmt.Sprintf("$%.2f", *usage.CostUSD)
		}
		field(w, "cost", cost)
	}

	fmt.Fprintf(w, "\nProgress (%d)\n", len(st.Progress))
	if len(st.Progress) == 0 {
		fmt.Fprintln(w, "  none")
		return 0
	}
	tw := newIndentedTable(w)
	for _, p := range st.Progress {
		data := ""
		if len(p.Data) > 0 {
			data = oneLineJSON(p.Data)
		}
		row(tw, ts(p.At), p.Kind, p.Message, data)
	}
	_ = tw.Flush()
	return 0
}

// resultLines prints a Result in full.
func resultLines(w io.Writer, res *v1alpha1.Result) {
	field(w, "status", string(res.Status))
	field(w, "outcome", res.Outcome)
	field(w, "summary", res.Summary)
	if res.Question != nil {
		field(w, "question", *res.Question)
	}
	field(w, "verdict", mapLines(res.Verdict))
	var arts []string
	for _, a := range res.Artifacts {
		arts = append(arts, joinNonEmpty(" ", a.Kind, a.URL, a.Ref))
	}
	field(w, "artifacts", strings.Join(arts, "\n"))
	var sugg []string
	for _, s := range res.Suggest {
		line := joinNonEmpty(": ", s.TaskSpec, s.Reason)
		if len(s.Inputs) > 0 {
			line += " " + oneLineJSON(s.Inputs)
		}
		sugg = append(sugg, line)
	}
	field(w, "suggest", strings.Join(sugg, "\n"))
	if h := res.Harness; h != nil {
		field(w, "harness", joinNonEmpty(", ", joinNonEmpty(" ", h.Name, h.Version, paren(h.Auth)),
			prefixed("model ", h.EffectiveConfig.Model), prefixed("effort ", h.EffectiveConfig.ReasoningEffort)))
	}
	field(w, "data", mapLines(res.Data))
}

// ---- workitem ----------------------------------------------------------------------------------

// workItemView is factory state workitem --json.
type workItemView struct {
	WorkItem v1alpha1.WorkItem `json:"workItem"`
	Tasks    []v1alpha1.Task   `json:"tasks"`
}

func stateWorkItem(ctx context.Context, r *stateRun, args []string) int {
	fl := r.flags("WORKITEM", true)
	pos, code, ok := r.parse(fl, args, 1)
	if !ok {
		return code
	}
	if code, ok := r.open(); !ok {
		return code
	}
	var wv workItemView
	if code, ok := r.get(ctx, v1alpha1.KindWorkItem, pos[0], &wv.WorkItem); !ok {
		return code
	}
	wi := &wv.WorkItem
	if _, err := r.view.List(ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskWorkItem: wi.Metadata.Name}}, &wv.Tasks); err != nil {
		return r.fail(err)
	}
	sortTasks(wv.Tasks)
	if r.json {
		wv.Tasks = nonNil(wv.Tasks)
		return r.writeJSON(wv)
	}

	w := r.stdout
	st := wi.Status
	if st == nil {
		st = &v1alpha1.WorkItemStatus{}
	}
	fmt.Fprintf(w, "WorkItem %s\n", wi.Metadata.Name)
	field(w, "created", ts(wi.Metadata.CreatedAt))
	field(w, "labels", labelLine(wi.Metadata.Labels))
	if st.Reporter != nil {
		field(w, "reporter", st.Reporter.ID)
	}

	fmt.Fprintf(w, "\nLinks (%d)\n", len(wi.Spec.Links))
	tw := newIndentedTable(w)
	for _, l := range wi.Spec.Links {
		primary := ""
		if l.Primary {
			primary = "primary"
		}
		row(tw, l.Provider, l.Ref, l.Kind, primary)
	}
	_ = tw.Flush()

	fmt.Fprintln(w, "\nState")
	if len(st.State) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, k := range slices.Sorted(maps.Keys(st.State)) {
		fmt.Fprintf(w, "  %s = %s\n", cell(k), oneLineJSON(st.State[k]))
	}
	fmt.Fprintln(w, "\nCounters")
	if len(st.Counters) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, k := range slices.Sorted(maps.Keys(st.Counters)) {
		fmt.Fprintf(w, "  %s = %d\n", cell(k), st.Counters[k])
	}
	if len(st.Approvals) > 0 {
		fmt.Fprintln(w, "\nApprovals")
		for _, a := range st.Approvals {
			fmt.Fprintf(w, "  %s  %s\n", cell(a.Name), a.Phase)
		}
	}

	fmt.Fprintf(w, "\nDecisions (%d, oldest first)\n", len(st.Decisions))
	if len(st.Decisions) > 0 {
		tw := newIndentedTable(w)
		row(tw, "TIME", "RULESET", "RULE", "EVENT", "VERDICT", "ACTIONS")
		for _, d := range st.Decisions {
			row(tw, ts(d.At), d.RuleSet, d.Rule, d.Event, verdict(&d), strings.Join(d.Actions, "; "))
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(w, "\nTasks (%d)\n", len(wv.Tasks))
	if len(wv.Tasks) > 0 {
		runs, err := runsByTask(ctx, r.view)
		if err != nil {
			return r.fail(err)
		}
		taskTable(&indentWriter{w: w, prefix: "  ", bol: true}, wv.Tasks, runs, false)
	}
	return 0
}

// ---- small helpers -----------------------------------------------------------------------------

// paren is "(s)", or "" for "".
func paren(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// prefixed is p+s, or "" for "".
func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func nonZero(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), sep)
}

// mapLines is m as "key = JSON" lines, sorted by key.
func mapLines(m map[string]any) string {
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		lines = append(lines, k+" = "+oneLineJSON(m[k]))
	}
	return strings.Join(lines, "\n")
}

func labelLine(labels map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}
