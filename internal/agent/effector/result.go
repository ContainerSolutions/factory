package effector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/pipeline"
)

// runResult validates a Result (a file, or the MCP tool's object) with the pipeline's own
// check (the v1alpha1 Result schema, the task's output schema and outcomes) and copies it to
// $FACTORY_RESULT. An invalid Result is refused (exit 4) with its problems, so the agent can
// fix it before the run ends rather than in the repair turn.
func runResult(_ context.Context, e *Effector, a args) (map[string]any, error) {
	file, obj := a.str("file"), a["result"]
	var doc []byte
	switch {
	case file != "" && obj != nil:
		return nil, usagef("result: give a file or a result, not both")
	case file != "":
		b, err := readInput(file, nil)
		if err != nil {
			return nil, usagef("result: %v", err)
		}
		doc = b
	case obj != nil:
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, usagef("result: %v", err)
		}
		doc = b
	default:
		return nil, usagef("result: give the Result file (--file)")
	}
	if e.o.Result == "" {
		return nil, usagef("result: $%s is not set: factory runs inside a factory task", pipeline.EnvResult)
	}
	task, spec, outcomes, specName := e.o.TaskID, []byte(nil), []string(nil), ""
	if c := e.o.Config; c != nil {
		outcomes, specName = c.Outcomes, c.TaskSpec
		if c.Schema != "" {
			b, err := os.ReadFile(c.Schema)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, &Error{Exit: ExitRejected, Msg: fmt.Sprintf("result: read the output schema: %v", err), Err: err}
			}
			spec = b
		}
	}
	if problems := pipeline.ValidateResult(task, specName, spec, outcomes, doc); len(problems) > 0 {
		return nil, &Error{Exit: ExitRejected, Extra: map[string]any{"problems": problems},
			Msg: "the result is invalid:\n  " + strings.Join(problems, "\n  ")}
	}
	if file == "" || !sameFile(file, e.o.Result) {
		if err := writeAtomic(e.o.Result, doc); err != nil {
			return nil, &Error{Exit: ExitRejected, Msg: fmt.Sprintf("result: write $%s: %v", pipeline.EnvResult, err), Err: err}
		}
	}
	var r struct {
		Status  string `json:"status"`
		Outcome string `json:"outcome,omitempty"`
	}
	_ = json.NewDecoder(bytes.NewReader(doc)).Decode(&r)
	out := map[string]any{"result": e.o.Result, "status": r.Status}
	if r.Outcome != "" {
		out["outcome"] = r.Outcome
	}
	return out, nil
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

// writeAtomic writes b to p through a temporary file in its directory.
func writeAtomic(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".result-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
