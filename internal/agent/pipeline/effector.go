package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// EnvEffector names the variable that carries the path of the run's EffectorConfig, which
// the Effector CLI and MCP server (`factory`, effector.md) read.
const EnvEffector = "FACTORY_EFFECTOR"

// EffectorFile is the EffectorConfig's name in the workspace root (outside out/, which the
// harness writes).
const EffectorFile = "effector.json"

// EffectorConfig is what the Effector knows about the run's task besides the FACTORY_*
// variables: the policy for its local pre-check (the dispatcher re-checks it and is
// authoritative), the repo bare refs (#12) resolve in, the checkout its git ops run in, and
// what `factory result` validates against.
type EffectorConfig struct {
	Task     string `json:"task"`
	TaskSpec string `json:"taskSpec,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Repo is the task's target repo (owner/name).
	Repo string `json:"repo,omitempty"`
	// Allow is the task's effects.allow (empty: nothing is allowed); Labels its
	// effects.labels globs.
	Allow  []string `json:"allow"`
	Labels []string `json:"labels,omitempty"`
	// Outcomes are the task's output.outcomes.
	Outcomes []string `json:"outcomes,omitempty"`
	// RepoDir is the primary checkout (repo/), where git ops run; empty without one.
	RepoDir string `json:"repoDir,omitempty"`
	// Schema is the file the Result must match (the spec's schema, else the Result schema).
	Schema string `json:"schema,omitempty"`
}

// EffectorConfigFor is the EffectorConfig of a run of t in ws.
func EffectorConfigFor(t v1alpha1.Task, ws harness.Workspace) EffectorConfig {
	c := EffectorConfig{Task: t.Metadata.ID, TaskSpec: t.Spec.TaskSpec.Name, Provider: t.Spec.Provider, Allow: []string{},
		RepoDir: ws.Repo, Schema: ws.Schema}
	if tg := t.Spec.Target; tg != nil {
		if tg.Provider != "" {
			c.Provider = tg.Provider
		}
		c.Repo = tg.Repo
		if c.Repo == "" {
			c.Repo, _, _ = strings.Cut(tg.Ref, "#")
		}
	}
	if cr := t.Spec.Credentials; c.Repo == "" && cr != nil && cr.Git != nil {
		c.Repo = cr.Git.Repo
	}
	if e := t.Spec.Effects; e != nil {
		for _, op := range e.Allow {
			c.Allow = append(c.Allow, string(op))
		}
		c.Labels = append(c.Labels, e.Labels...)
	}
	if o := t.Spec.Output; o != nil {
		c.Outcomes = append(c.Outcomes, o.Outcomes...)
	}
	return c
}

// writeEffectorConfig writes the EffectorConfig to the workspace root and returns its path.
func (r *run) writeEffectorConfig() (string, error) {
	b, err := json.MarshalIndent(EffectorConfigFor(r.task, r.ws), "", "  ")
	if err != nil {
		return "", fmt.Errorf("the effector config: %w", err)
	}
	p := filepath.Join(r.ws.Root, EffectorFile)
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("the effector config: %w", err)
	}
	return p, nil
}

// ValidateResult checks a Result document as the pipeline will after the run: the v1alpha1
// Result schema, the spec schema (nil: none), the status and the outcome against outcomes.
// taskSpec names the spec schema's $id. It returns the problems ("/<pointer>: message"), none
// when the document is valid. `factory result` uses it for fast feedback; the pipeline's own
// check (with the harness block filled in) still decides.
func ValidateResult(taskID, taskSpec string, spec []byte, outcomes []string, doc []byte) []string {
	generic, err := resultSchema()
	if err != nil {
		return []string{err.Error()}
	}
	chk := &resultCheck{taskID: taskID, generic: generic, outcomes: outcomes}
	// Without a spec schema the workspace's schema file is the Result schema itself.
	if gen, err := v1alpha1Schemas(); err == nil && bytes.Equal(bytes.TrimSpace(spec), bytes.TrimSpace(gen[v1alpha1.ResultSchemaFile])) {
		spec = nil
	}
	if spec != nil {
		if chk.spec, err = compileSpecSchema(taskSpec, spec); err != nil {
			return []string{err.Error()}
		}
	}
	_, problems := chk.check(doc, nil)
	return problems
}

// compileSpecSchema compiles a TaskSpec's output schema with the v1alpha1 schemas known.
func compileSpecSchema(taskSpec string, spec []byte) (*jsonschema.Schema, error) {
	c, err := newSchemaCompiler()
	if err != nil {
		return nil, err
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(spec))
	if err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	u := outputSchemaURL + taskSpec + ".json"
	if err := c.AddResource(u, parsed); err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	s, err := c.Compile(u)
	if err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	return s, nil
}
