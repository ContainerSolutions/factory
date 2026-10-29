package controllers

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// What a Rendered text is.
const (
	RenderPrompt   = "prompt"   // a dispatch's prompt; Name is the TaskSpec
	RenderTemplate = "template" // a notify's GitHub body; Name is the Template
)

// RenderUIBase is the Factory UI base URL of links.ui in RenderEvaluation's Templates.
const RenderUIBase = "https://factory.example"

// Rendered is the text one decision of an evaluation renders.
type Rendered struct {
	// Decision is the decision's index in the evaluation.
	Decision int
	// Kind is RenderPrompt or RenderTemplate.
	Kind string
	// Name is the TaskSpec (RenderPrompt) or the Template (RenderTemplate).
	Name string
	Text string
	// Err is why it doesn't render; Text is empty then.
	Err error
}

// RenderEvaluation renders what the rule controller renders when it applies eval's decisions
// for ev to the work item view, with no store, provider or network (`factory rules test`):
// the prompt of each dispatch whose TaskSpec has one (compileTask with KeepSkills, the task id
// the decision's key gives, ev's time as now) and the GitHub body of each notify's Template
// (templateData with RenderUIBase, renderBody). The decisions apply in order as apply does:
// an allowed or deferred set or link changes the work item the later decisions see. A dispatch
// or a notify renders whatever its rail verdict, so a suppressed one is checked too.
func RenderEvaluation(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event, view rules.WorkItemView, eval rules.Evaluation) []Rendered {
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: view.ID},
		Spec:     v1alpha1.WorkItemSpec{Links: slices.Clone(view.Links)},
		Status:   &v1alpha1.WorkItemStatus{State: maps.Clone(view.State), Counters: maps.Clone(view.Counters)},
	}
	ac := &applyCtx{cat: cat, ev: ev, wi: wi, now: ev.Time}
	var out []Rendered
	perRule := map[string]int{}
	for i, d := range eval.Decisions {
		n := perRule[d.Rule]
		perRule[d.Rule]++
		switch act := d.Action; {
		case act.Dispatch != nil:
			ts := cat.TaskSpecs[act.Dispatch.TaskSpec]
			if ts != nil && (ts.Prompt == nil || ts.Prompt.Template == nil) {
				continue
			}
			r := Rendered{Decision: i, Kind: RenderPrompt, Name: act.Dispatch.TaskSpec}
			_, prompt, err := compileTask(ctx, compileInput{
				cat: cat, d: act.Dispatch, wi: wi, ev: ev, ruleSet: d.RuleSet, rule: d.Rule,
				taskID: objectID("tsk", fmt.Sprintf("evt/%s/%s/%d", ev.ID, d.Rule, n)), now: ev.Time,
			}, KeepSkills{})
			if err != nil {
				r.Err = err
			} else {
				r.Text = prompt.Text
			}
			out = append(out, r)
		case act.Notify != nil:
			r := Rendered{Decision: i, Kind: RenderTemplate, Name: act.Notify.Template}
			if t := cat.Templates[act.Notify.Template]; t == nil {
				r.Err = fmt.Errorf("%w: template %q does not exist", errNotify, act.Notify.Template)
			} else {
				r.Text, r.Err = renderBody(t, v1alpha1.ProviderTypeGitHub, templateData(ev, wi, RenderUIBase))
			}
			out = append(out, r)
		case applies(d.Rail):
			applyToItem(ac, act)
		}
	}
	return out
}

// resultFields are the JSON names of v1alpha1.Result's fields.
var resultFields = func() map[string]bool {
	m := map[string]bool{}
	t := reflect.TypeFor[v1alpha1.Result]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		m[name] = true
	}
	return m
}()

// CheckRunResult validates the Result a factory.run.completed event's run fact carries against
// its TaskSpec's output schema in cat, as complete does. The Result is run.result read back
// (resultMap's inverse): its Result fields as they are, the others under data unless it has
// data, and the run's id, status and outcome where it lacks task, status and outcome. It
// returns the TaskSpec it checked against ("" when ev is another event or has no run.result)
// and the schema's error; a TaskSpec cat doesn't have is an error.
func CheckRunResult(cat *catalog.Catalog, ev v1alpha1.Event) (string, error) {
	if ev.Type != v1alpha1.EventFactoryRunCompleted {
		return "", nil
	}
	run, _ := ev.Data.Facts["run"].(map[string]any)
	view, ok := run["result"].(map[string]any)
	if !ok {
		return "", nil
	}
	spec, _ := run["spec"].(string)
	ts := cat.TaskSpecs[spec]
	if ts == nil {
		return spec, fmt.Errorf("TaskSpec %q is %w", spec, ErrNotInCatalog)
	}
	res := map[string]any{"task": run["id"], "status": run["status"], "question": nil}
	if o, _ := run["outcome"].(string); o != "" {
		res["outcome"] = o
	}
	data := map[string]any{}
	for k, v := range view {
		switch {
		case resultFields[k]:
			res[k] = v
		default:
			data[k] = v
		}
	}
	if _, has := view["data"]; !has && len(data) > 0 {
		res["data"] = data
	}
	return spec, checkOutputSchema(ts, res)
}
