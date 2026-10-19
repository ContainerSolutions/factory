package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// TestTaskOutputSchema: getTaskOutputSchema serves the TaskSpec's output schema from the live
// catalog, byte for byte, to the lease holder and to UI/CLI readers; 404 when the TaskSpec
// declares none (or is gone) or the task doesn't exist.
func TestTaskOutputSchema(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	raw := e.cat.TaskSpecs["implement"].OutputSchemaDocument
	if len(raw) == 0 {
		t.Fatal("the PoC implement TaskSpec has no output schema")
	}

	noSchema := *e.cat.TaskSpecs["implement"]
	noSchema.OutputSchema, noSchema.OutputSchemaDocument = nil, nil
	e.cat.TaskSpecs["no-schema"] = &noSchema

	// A Pending task, read by the CLI and by a reader's session.
	e.task("tsk_1", nil)
	// tsk_2 is on a queue no agent serves, so the lease below takes tsk_1.
	e.task("tsk_2", func(t *v1alpha1.Task) { t.Spec.TaskSpec.Name = "no-schema"; t.Spec.Queue = "elsewhere" })
	for name, cred := range map[string]func() []byte{
		"cli": func() []byte {
			r, err := e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_1", bearer(cliToken))
			if err != nil || r.StatusCode() != http.StatusOK {
				t.Fatalf("cli: %v %s %s", err, r.Status(), r.Body)
			}
			if ct := r.HTTPResponse.Header.Get("Content-Type"); ct != MediaTypeSchemaJSON {
				t.Errorf("content type %q", ct)
			}
			return r.Body
		},
		"reader": func() []byte {
			r, err := e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_1", session(e.login("rita")))
			if err != nil || r.StatusCode() != http.StatusOK {
				t.Fatalf("reader: %v %s %s", err, r.Status(), r.Body)
			}
			return r.Body
		},
	} {
		if got := cred(); !bytes.Equal(got, raw) {
			t.Errorf("%s: %s, want the catalog's document %s", name, got, raw)
		}
	}

	// The lease holder; then its TaskSpec leaves the catalog (as after a reload): 404, the
	// case where complete checks the outcomes only.
	agent, tok := e.register("gus", "mac1")
	if l := e.lease(agent, tok); l.Task.Metadata.ID != "tsk_1" {
		t.Fatalf("leased %s", l.Task.Metadata.ID)
	}
	r, err := e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_1", bearer(tok))
	if err != nil || r.StatusCode() != http.StatusOK || !bytes.Equal(r.Body, raw) {
		t.Fatalf("lease holder: %v %s %s", err, r.Status(), r.Body)
	}
	tk := e.getTask("tsk_1")
	tk.Spec.TaskSpec.Name = "gone"
	if err := e.st.Update(ctx, tk); err != nil {
		t.Fatal(err)
	}
	r, err = e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_1", bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a TaskSpec the catalog doesn't have", r.HTTPResponse, r.Body, http.StatusNotFound, CodeNotFound)

	// A TaskSpec without output.schema (added before the task is stored: the store's lock
	// orders the write before the handler's read).
	r, err = e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_2", bearer(cliToken))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a TaskSpec without output.schema", r.HTTPResponse, r.Body, http.StatusNotFound, CodeNotFound)

	r, err = e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_x", bearer(cliToken))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "no such task", r.HTTPResponse, r.Body, http.StatusNotFound, CodeNotFound)
}
