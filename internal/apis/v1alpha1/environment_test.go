package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// p1Example is the P1 §4 example as a TaskSpec, with versions and egress in their object form.
const p1Example = `kind: TaskSpec
metadata: { name: docs-gif }
spec:
  workspace:
    repos:
      - { ref: "=task.target.repo + '@main'", mode: rw }
      - { ref: compliance-framework/local-dev@main, mode: ro }
    environment:
      versions: { from: latest }
      setup: |
        corepack enable
      up: |
        make -C /work/ro/local-dev todo-agent-env
        cd /work/ro/local-dev && ./scripts/do start_showcase
        make -C /work/ro/local-dev demo-up-showcase-load showcase-kind register-todo-agent
      exposes: { FACTORY_STACK_URL: "http://localhost:8000", FACTORY_STACK_API: "http://localhost:8080" }
      ready: { http: ["http://localhost:8080/api/health", "http://localhost:8000/"], timeout: 20m }
      down: |
        make -C /work/ro/local-dev compose-down-showcase
      egress: { mode: default }
`

func many(n int, format string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(format, i)
	}
	return out
}

func manyMap(n int, key, value string) map[string]any {
	out := make(map[string]any, n)
	for i := range n {
		out[fmt.Sprintf(key, i)] = value
	}
	return out
}

// envCases are workspace.environment blocks with the field path (under the environment) and
// sentinel Validate reports, nil for a valid one. TaskSpecs carrying them declare one input,
// "train".
var envCases = []struct {
	name  string
	env   map[string]any
	field string
	want  error
}{
	{"empty", map[string]any{}, "", nil},
	{"up only", map[string]any{"up": "make up"}, "", nil},
	{"versions latest", map[string]any{"versions": map[string]any{"from": "latest"}}, "", nil},
	{"versions inputs", map[string]any{"versions": map[string]any{"from": "inputs", "input": "train"}}, "", nil},
	{"versions train", map[string]any{"versions": map[string]any{"from": "train", "input": "train"}}, "", nil},
	{"versions from unknown", map[string]any{"versions": map[string]any{"from": "nightly"}}, "versions.from", ErrEnvVersionsFrom},
	{"versions from empty", map[string]any{"versions": map[string]any{"from": ""}}, "versions.from", ErrEnvVersionsFrom},
	{"versions latest with input", map[string]any{"versions": map[string]any{"from": "latest", "input": "train"}}, "versions.input", ErrEnvVersionsInput},
	{"versions inputs without input", map[string]any{"versions": map[string]any{"from": "inputs"}}, "versions.input", ErrEnvVersionsInput},
	{"versions train without input", map[string]any{"versions": map[string]any{"from": "train"}}, "versions.input", ErrEnvVersionsInput},

	{"setup at the limit", map[string]any{"setup": strings.Repeat("a", MaxEnvScriptBytes)}, "", nil},
	{"setup too long", map[string]any{"setup": strings.Repeat("a", MaxEnvScriptBytes+1)}, "setup", ErrEnvScriptTooLong},
	{"up too long", map[string]any{"up": strings.Repeat("a", MaxEnvScriptBytes+1)}, "up", ErrEnvScriptTooLong},
	{"down too long", map[string]any{"down": strings.Repeat("a", MaxEnvScriptBytes+1)}, "down", ErrEnvScriptTooLong},

	{"ready without up", map[string]any{"ready": map[string]any{"http": []string{"http://localhost:8080/"}}}, "ready", ErrEnvNoUp},
	{"exposes without up", map[string]any{"exposes": map[string]any{"FACTORY_STACK_URL": "http://localhost:8000"}}, "exposes", ErrEnvNoUp},

	{"ready https and timeout", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"https://localhost:8443/x?y=1"}, "timeout": "60m"}}, "", nil},
	{"ready at the limit", map[string]any{"up": "u", "ready": map[string]any{"http": many(MaxEnvReadyURLs, "http://localhost:%d/")}}, "", nil},
	{"ready no URLs", map[string]any{"up": "u", "ready": map[string]any{"timeout": "5m"}}, "ready.http", ErrEnvReadyURL},
	{"ready too many URLs", map[string]any{"up": "u", "ready": map[string]any{"http": many(MaxEnvReadyURLs+1, "http://localhost:%d/")}}, "ready.http", ErrEnvReadyURL},
	{"ready URL scheme", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://localhost/", "ftp://localhost/"}}}, "ready.http[1]", ErrEnvReadyURL},
	{"ready URL relative", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"/api/health"}}}, "ready.http[0]", ErrEnvReadyURL},
	{"ready URL no host", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http:///health"}}}, "ready.http[0]", ErrEnvReadyURL},
	{"ready URL empty host", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://:8080/"}}}, "ready.http[0]", ErrEnvReadyURL},
	{"ready URL space", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://localhost/a b"}}}, "ready.http[0]", ErrEnvReadyURL},
	{"ready timeout zero", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://localhost/"}, "timeout": "0s"}}, "ready.timeout", ErrEnvReadyTimeout},
	{"ready timeout over the limit", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://localhost/"}, "timeout": "61m"}}, "ready.timeout", ErrEnvReadyTimeout},
	{"ready timeout invalid", map[string]any{"up": "u", "ready": map[string]any{"http": []string{"http://localhost/"}, "timeout": "soon"}}, "ready.timeout", ErrEnvReadyTimeout},

	{"exposes at the limit", map[string]any{"up": "u", "exposes": manyMap(MaxEnvExposes, "FACTORY_X%d", "v")}, "", nil},
	{"exposes too many", map[string]any{"up": "u", "exposes": manyMap(MaxEnvExposes+1, "FACTORY_X%d", "v")}, "exposes", ErrEnvExposeName},
	{"exposes no prefix", map[string]any{"up": "u", "exposes": map[string]any{"STACK_URL": "v"}}, "exposes.STACK_URL", ErrEnvExposeName},
	{"exposes lowercase", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_stack": "v"}}, "exposes.FACTORY_stack", ErrEnvExposeName},
	{"exposes prefix only", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_": "v"}}, "exposes.FACTORY_", ErrEnvExposeName},
	{"exposes reserved", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_TOKEN": "v"}}, "exposes.FACTORY_TOKEN", ErrEnvExposeName},
	{"exposes version", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_VERSION_API": "v"}}, "exposes.FACTORY_VERSION_API", ErrEnvExposeName},
	{"exposes empty value", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_X": ""}}, "exposes.FACTORY_X", ErrEnvExposeValue},
	{"exposes value at the limit", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_X": strings.Repeat("v", MaxEnvExposeValueBytes)}}, "", nil},
	{"exposes value too long", map[string]any{"up": "u", "exposes": map[string]any{"FACTORY_X": strings.Repeat("v", MaxEnvExposeValueBytes+1)}}, "exposes.FACTORY_X", ErrEnvExposeValue},

	{"egress default", map[string]any{"egress": map[string]any{"mode": "default"}}, "", nil},
	{"egress deny-by-default", map[string]any{"egress": map[string]any{"mode": "deny-by-default", "allow": []string{"registry.npmjs.org", "*.docker.io"}}}, "", nil},
	{"egress mode unknown", map[string]any{"egress": map[string]any{"mode": "closed"}}, "egress.mode", ErrEnvEgressMode},
	{"egress allow with default", map[string]any{"egress": map[string]any{"mode": "default", "allow": []string{"registry.npmjs.org"}}}, "egress.allow", ErrEnvEgressAllow},
	{"egress allow without mode", map[string]any{"egress": map[string]any{"allow": []string{"registry.npmjs.org"}}}, "egress.allow", ErrEnvEgressAllow},
	{"egress allow localhost", map[string]any{"egress": map[string]any{"mode": "deny-by-default", "allow": []string{"localhost:8080"}}}, "egress.allow[0]", ErrHostLocalhost},
	{"egress allow too many", map[string]any{"egress": map[string]any{"mode": "deny-by-default", "allow": many(MaxWorkspaceHosts+1, "h%d.example.com")}}, "egress.allow", ErrTooManyHosts},
}

func taskSpecWithEnv(t *testing.T, env map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"kind": KindTaskSpec, "metadata": map[string]any{"name": "t"}, "spec": map[string]any{
		"inputs":    map[string]any{"train": map[string]any{"type": "string"}},
		"workspace": map[string]any{"environment": env},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func taskWithEnv(t *testing.T, env map[string]any, versions map[string]any) []byte {
	t.Helper()
	spec := map[string]any{"taskSpec": map[string]any{"name": "ts"}, "queue": "q"}
	if env != nil {
		spec["environment"] = env
	}
	if versions != nil {
		spec["versions"] = versions
	}
	b, err := json.Marshal(map[string]any{"kind": KindTask, "metadata": map[string]any{"id": "t1", "workItem": "w"}, "spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkValidation(t *testing.T, err error, field string, want error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("want valid, got %v", err)
		}
		return
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a *ValidationError, got %v", err)
	}
	if ve.Field != field {
		t.Errorf("field = %q, want %q", ve.Field, field)
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func joinField(prefix, field string) string {
	if field == "" {
		return ""
	}
	return prefix + "." + field
}

func TestTaskSpecValidateEnvironment(t *testing.T) {
	for _, tt := range envCases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(taskSpecWithEnv(t, tt.env))
			checkValidation(t, err, joinField("spec.workspace.environment", tt.field), tt.want)
		})
	}
}

func TestTaskValidateEnvironment(t *testing.T) {
	for _, tt := range envCases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(taskWithEnv(t, tt.env, nil))
			checkValidation(t, err, joinField("spec.environment", tt.field), tt.want)
		})
	}
}

func TestTaskSpecVersionsInputNamesAnInput(t *testing.T) {
	tests := []struct {
		name, from, input string
		want              error
	}{
		{"declared", "train", "train", nil},
		{"undeclared train", "train", "release", ErrEnvVersionsInput},
		{"undeclared inputs", "inputs", "versions", ErrEnvVersionsInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(taskSpecWithEnv(t, map[string]any{"versions": map[string]any{"from": tt.from, "input": tt.input}}))
			checkValidation(t, err, "spec.workspace.environment.versions.input", tt.want)
		})
	}
	// A Task has no declared inputs: Task.Validate doesn't check the name.
	if _, err := Decode(taskWithEnv(t, map[string]any{"versions": map[string]any{"from": "train", "input": "release"}}, nil)); err != nil {
		t.Errorf("task: %v", err)
	}
}

func TestTaskSpecP1Example(t *testing.T) {
	objs, err := Decode([]byte(p1Example))
	if err != nil {
		t.Fatal(err)
	}
	env := objs[0].(*TaskSpec).Spec.Workspace.Environment
	if env == nil || env.Versions.From != "latest" || env.Ready.Timeout != "20m" || len(env.Ready.HTTP) != 2 ||
		env.Exposes["FACTORY_STACK_URL"] != "http://localhost:8000" || env.Egress.Mode != "default" ||
		!strings.HasPrefix(env.Up, "make -C /work/ro/local-dev todo-agent-env\n") {
		t.Errorf("environment = %+v", env)
	}
}

var versionsCases = []struct {
	name     string
	versions map[string]any
	field    string
	want     error
}{
	{"valid", map[string]any{"api": "v0.12.0", "ui": "v0.9.1", "plugin-local-ssh": "sha-1a2b3c"}, "", nil},
	{"at the limit", manyMap(MaxVersions, "c%d", "v1"), "", nil},
	{"too many", manyMap(MaxVersions+1, "c%d", "v1"), "spec.versions", ErrEnvVersions},
	{"uppercase component", map[string]any{"API": "v1"}, "spec.versions.API", ErrEnvVersions},
	{"leading dash", map[string]any{"-api": "v1"}, "spec.versions.-api", ErrEnvVersions},
	{"underscore", map[string]any{"local_ssh": "v1"}, "spec.versions.local_ssh", ErrEnvVersions},
	{"component too long", map[string]any{strings.Repeat("a", 64): "v1"}, "spec.versions." + strings.Repeat("a", 64), ErrEnvVersions},
	{"component at the limit", map[string]any{strings.Repeat("a", 63): "v1"}, "", nil},
	{"empty version", map[string]any{"api": ""}, "spec.versions.api", ErrEnvVersions},
	{"version at the limit", map[string]any{"api": strings.Repeat("v", MaxVersionBytes)}, "", nil},
	{"version too long", map[string]any{"api": strings.Repeat("v", MaxVersionBytes+1)}, "spec.versions.api", ErrEnvVersions},
}

func TestTaskValidateVersions(t *testing.T) {
	for _, tt := range versionsCases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(taskWithEnv(t, nil, tt.versions))
			checkValidation(t, err, tt.field, tt.want)
		})
	}
}

func TestTaskWithEnvironmentAndVersions(t *testing.T) {
	objs, err := Decode(taskWithEnv(t,
		map[string]any{"versions": map[string]any{"from": "latest"}, "up": "make up",
			"exposes": map[string]any{"FACTORY_STACK_URL": "http://localhost:8000"},
			"ready":   map[string]any{"http": []string{"http://localhost:8000/"}, "timeout": "20m"}},
		map[string]any{"api": "v0.12.0", "ui": "v0.9.1"}))
	if err != nil {
		t.Fatal(err)
	}
	spec := objs[0].(*Task).Spec
	if spec.Environment == nil || spec.Environment.Up != "make up" || spec.Versions["api"] != "v0.12.0" {
		t.Errorf("spec = %+v", spec)
	}
}

// TestEnvironmentSchemaMatchesGo checks that taskspec.schema.json and the Task's schema agree
// with Validate on every case the schema can express: the byte limits are only Validate's (the
// schema counts characters), and so are the rules across fields (ready and exposes need up,
// versions.input against versions.from, allow only with deny-by-default) and the timeout.
func TestEnvironmentSchemaMatchesGo(t *testing.T) {
	taskSpecSchema := compileSchemas(t)["taskspec.schema.json"]
	taskSchema := taskSchema(t)
	goOnly := map[error]bool{ErrEnvNoUp: true, ErrEnvVersionsInput: true, ErrEnvEgressAllow: true, ErrEnvReadyTimeout: true}
	for _, tt := range envCases {
		if goOnly[tt.want] || tt.name == "ready URL empty host" {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			ts, err := jsonschema.UnmarshalJSON(bytes.NewReader(taskSpecWithEnv(t, tt.env)))
			if err != nil {
				t.Fatal(err)
			}
			if schemaOK := taskSpecSchema.Validate(ts) == nil; schemaOK != (tt.want == nil) {
				t.Errorf("taskspec schema accepts = %v, Validate accepts = %v", schemaOK, tt.want == nil)
			}
			tk, err := jsonschema.UnmarshalJSON(bytes.NewReader(taskWithEnv(t, tt.env, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if schemaOK := taskSchema.Validate(tk) == nil; schemaOK != (tt.want == nil) {
				t.Errorf("task schema accepts = %v, Validate accepts = %v", schemaOK, tt.want == nil)
			}
		})
	}
	for _, tt := range versionsCases {
		t.Run("versions "+tt.name, func(t *testing.T) {
			tk, err := jsonschema.UnmarshalJSON(bytes.NewReader(taskWithEnv(t, nil, tt.versions)))
			if err != nil {
				t.Fatal(err)
			}
			if schemaOK := taskSchema.Validate(tk) == nil; schemaOK != (tt.want == nil) {
				t.Errorf("task schema accepts = %v, Validate accepts = %v", schemaOK, tt.want == nil)
			}
		})
	}
}

// taskSchema compiles the Task's schema, the one the OpenAPI components are reflected from.
func taskSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := json.Marshal(NewReflector().Reflect(&Task{}))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("task.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("task.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return sch
}
