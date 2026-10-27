package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// maxResult bounds a Result document; maxProblems the problems reported for one.
const (
	maxResult   = 1 << 20
	maxProblems = 20
)

// outputSchemaURL is the $id a task's spec-specific schema is compiled under.
const outputSchemaURL = "https://factory.ccf.dev/task-output/"

// v1alpha1Schemas are the generated v1alpha1 schemas, by file name.
var v1alpha1Schemas = sync.OnceValues(v1alpha1.Schemas)

// newSchemaCompiler returns a compiler that knows the v1alpha1 schemas by their $id and
// loads nothing else (no file system, no network).
func newSchemaCompiler() (*jsonschema.Compiler, error) {
	gen, err := v1alpha1Schemas()
	if err != nil {
		return nil, fmt.Errorf("the v1alpha1 schemas: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	names := make([]string, 0, len(gen))
	for n := range gen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen[n]))
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", n, err)
		}
		if err := c.AddResource(v1alpha1.SchemaBaseURL+n, doc); err != nil {
			return nil, fmt.Errorf("schema %s: %w", n, err)
		}
	}
	return c, nil
}

// resultSchema is the compiled v1alpha1 Result schema.
var resultSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	c, err := newSchemaCompiler()
	if err != nil {
		return nil, err
	}
	return c.Compile(v1alpha1.SchemaBaseURL + v1alpha1.ResultSchemaFile)
})

// resultCheck validates a run's Result: the v1alpha1 Result schema, the task's
// spec-specific schema when known, and the task's outcomes.
type resultCheck struct {
	taskID   string
	generic  *jsonschema.Schema
	spec     *jsonschema.Schema
	outcomes []string
}

// prepareSchema compiles the task's output schema and writes the schema the harness is
// given (the spec's when known, else the Result schema) to the workspace, self-contained
// (BundleSchema). The run's Result is checked against the original documents.
func (r *run) prepareSchema(ctx context.Context) error {
	generic, err := resultSchema()
	if err != nil {
		return err
	}
	gen, err := v1alpha1Schemas()
	if err != nil {
		return err
	}
	doc := gen[v1alpha1.ResultSchemaFile]
	chk := &resultCheck{taskID: r.id, generic: generic}
	if o := r.task.Spec.Output; o != nil {
		chk.outcomes = slices.Clone(o.Outcomes)
	}
	spec, err := r.p.o.Schemas.OutputSchema(ctx, r.job)
	if err != nil {
		return fmt.Errorf("the output schema: %w", err)
	}
	if spec != nil {
		if chk.spec, err = compileSpecSchema(r.task.Spec.TaskSpec.Name, spec); err != nil {
			return err
		}
		doc = spec
	}
	r.check = chk
	// The harness gets the schema self-contained: it can't resolve the Result schema's $id.
	if doc, err = BundleSchema(doc); err != nil {
		return err
	}
	if err := os.WriteFile(r.ws.Schema, doc, 0o600); err != nil {
		return fmt.Errorf("the output schema: %w", err)
	}
	return nil
}

// checkResult reads the run's Result ($FACTORY_RESULT, else the harness's structured
// output) and validates it.
func (r *run) checkResult(raw harness.Raw) (*v1alpha1.Result, []string) {
	b, err := readResultFile(r.ws.Result)
	if err != nil {
		return nil, []string{err.Error()}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = raw.Result
	}
	return r.check.check(b, r.harnessInfo(r.mon.totals(0)))
}

func readResultFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read $FACTORY_RESULT: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxResult+1))
	if err != nil {
		return nil, fmt.Errorf("read $FACTORY_RESULT: %w", err)
	}
	return b, nil
}

// check validates one Result document. The pipeline fills in what it knows better than the
// harness first: task (when missing), question (null when missing) and harness (always).
func (c *resultCheck) check(b []byte, h *v1alpha1.ResultHarness) (*v1alpha1.Result, []string) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, []string{"no result: write the Result as one JSON object to $FACTORY_RESULT"}
	}
	if len(b) > maxResult {
		return nil, []string{fmt.Sprintf("the result is over %d bytes", maxResult)}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, []string{"the result is not a JSON object"}
	}
	if dec.More() {
		return nil, []string{"the result has more than one JSON value"}
	}
	var problems []string
	switch t := doc["task"].(type) {
	case nil:
		doc["task"] = c.taskID
	case string:
		if t == "" {
			doc["task"] = c.taskID
		} else if t != c.taskID {
			problems = append(problems, fmt.Sprintf("task is %q, want %q", t, c.taskID))
		}
	}
	if _, ok := doc["question"]; !ok {
		doc["question"] = nil
	}
	if h != nil {
		doc["harness"] = h
	}
	enc, err := json.Marshal(doc)
	if err != nil {
		return nil, []string{"the result can't be encoded: " + err.Error()}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(enc))
	if err != nil {
		return nil, []string{"the result is not JSON: " + err.Error()}
	}
	problems = append(problems, schemaProblems(c.generic, inst)...)
	if c.spec != nil {
		problems = append(problems, schemaProblems(c.spec, inst)...)
	}
	status, _ := doc["status"].(string)
	outcome, _ := doc["outcome"].(string)
	question, _ := doc["question"].(string)
	problems = append(problems, c.outcomeProblems(v1alpha1.ResultStatus(status), outcome, question)...)
	var res *v1alpha1.Result
	if len(problems) == 0 {
		res, err = v1alpha1.DecodeResult(enc)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	problems = dedupe(problems)
	if len(problems) > maxProblems {
		problems = append(problems[:maxProblems], fmt.Sprintf("… and %d more", len(problems)-maxProblems))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return res, nil
}

// outcomeProblems checks the status, the outcome against the task's outcomes, and the
// question of a needs_input result (the dispatcher's own checks, controllers'
// validateResult).
func (c *resultCheck) outcomeProblems(status v1alpha1.ResultStatus, outcome, question string) []string {
	var out []string
	switch status {
	case v1alpha1.ResultDone, v1alpha1.ResultFailed, v1alpha1.ResultNeedsInput, v1alpha1.ResultNoop:
	default:
		out = append(out, fmt.Sprintf("/status: %q is not one of done, failed, needs_input, noop", status))
	}
	if len(c.outcomes) > 0 {
		switch {
		case outcome == "" && status == v1alpha1.ResultDone:
			out = append(out, fmt.Sprintf("/outcome: a done result needs an outcome (one of %s)", strings.Join(c.outcomes, ", ")))
		case outcome != "" && !slices.Contains(c.outcomes, outcome):
			out = append(out, fmt.Sprintf("/outcome: %q is not one of %s", outcome, strings.Join(c.outcomes, ", ")))
		}
	}
	if status == v1alpha1.ResultNeedsInput && strings.TrimSpace(question) == "" {
		out = append(out, "a needs_input result needs a question")
	}
	return out
}

// schemaProblems validates inst against s and returns the leaf violations as
// "<location>: <message>".
func schemaProblems(s *jsonschema.Schema, inst any) []string {
	err := s.Validate(inst)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			out = append(out, loc+": "+kindMessage(e.ErrorKind))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// kindMessage renders one error kind with the library's English printer.
func kindMessage(k jsonschema.ErrorKind) string {
	out := (&jsonschema.ValidationError{ErrorKind: k}).BasicOutput()
	if out.Error == nil {
		return "invalid"
	}
	return out.Error.String()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
