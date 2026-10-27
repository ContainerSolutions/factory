package v1alpha1

import (
	"encoding/json"
	"errors"
	"testing"
)

// limitsCases are spec.limits values with the sentinel Limits.Validate wraps (nil = valid) and
// the field it names. cpus 0 is unset in Go (omitzero) but explicit 0 fails the schema, so
// it isn't here: TestLimitsSchemaMatchesGo covers the schema on its own.
var limitsCases = []struct {
	name   string
	limits map[string]any
	field  string
	want   error
}{
	{"none", map[string]any{}, "", nil},
	{"run limits only", map[string]any{"maxTurns": 40, "maxWallClock": "15m"}, "", nil},
	{"cpus 1", map[string]any{"cpus": 1}, "", nil},
	{"cpus 4", map[string]any{"cpus": 4}, "", nil},
	{"memory 1Gi", map[string]any{"memory": "1Gi"}, "", nil},
	{"memory 16Gi", map[string]any{"cpus": 2, "memory": "16Gi"}, "", nil},
	{"memory 6 digits", map[string]any{"memory": "999999Gi"}, "", nil},
	{"cpus negative", map[string]any{"cpus": -1}, "cpus", ErrLimitsCPUs},
	{"memory 0Gi", map[string]any{"memory": "0Gi"}, "memory", ErrLimitsMemory},
	{"memory leading zero", map[string]any{"memory": "08Gi"}, "memory", ErrLimitsMemory},
	{"memory no suffix", map[string]any{"memory": "8"}, "memory", ErrLimitsMemory},
	{"memory G", map[string]any{"memory": "8G"}, "memory", ErrLimitsMemory},
	{"memory Mi", map[string]any{"memory": "512Mi"}, "memory", ErrLimitsMemory},
	{"memory fraction", map[string]any{"memory": "1.5Gi"}, "memory", ErrLimitsMemory},
	{"memory space", map[string]any{"memory": " 8Gi"}, "memory", ErrLimitsMemory},
	{"memory 7 digits", map[string]any{"memory": "1000000Gi"}, "memory", ErrLimitsMemory},
}

func taskSpecWithLimits(t *testing.T, limits map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"kind": KindTaskSpec, "metadata": map[string]any{"name": "t"},
		"spec": map[string]any{"limits": limits}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func taskWithLimits(t *testing.T, limits map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"kind": KindTask, "metadata": map[string]any{"id": "t1", "workItem": "w"},
		"spec": map[string]any{"taskSpec": map[string]any{"name": "ts"}, "queue": "q", "limits": limits}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestLimitsValidate decodes each case as a TaskSpec and as a Task: both validate their
// spec.limits the same way.
func TestLimitsValidate(t *testing.T) {
	for _, kind := range []struct {
		name string
		doc  func(*testing.T, map[string]any) []byte
	}{{"TaskSpec", taskSpecWithLimits}, {"Task", taskWithLimits}} {
		for _, tt := range limitsCases {
			t.Run(kind.name+"/"+tt.name, func(t *testing.T) {
				_, err := Decode(kind.doc(t, tt.limits))
				if tt.want == nil {
					if err != nil {
						t.Fatalf("want valid, got %v", err)
					}
					return
				}
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("want a *ValidationError, got %v", err)
				}
				if want := "spec.limits." + tt.field; ve.Field != want {
					t.Errorf("field = %q, want %q", ve.Field, want)
				}
				if !errors.Is(err, tt.want) {
					t.Errorf("err = %v, want %v", err, tt.want)
				}
			})
		}
	}
}

func TestLimitsDecodeSize(t *testing.T) {
	objs, err := Decode([]byte("kind: TaskSpec\nmetadata: { name: t }\nspec:\n  limits: { maxTurns: 40, cpus: 2, memory: 4Gi }\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := objs[0].(*TaskSpec).Spec.Limits
	if want := (Limits{MaxTurns: 40, CPUs: 2, Memory: "4Gi"}); *got != want {
		t.Errorf("limits = %+v, want %+v", *got, want)
	}
}

func TestNilLimitsValidate(t *testing.T) {
	var l *Limits
	if err := l.Validate(); err != nil {
		t.Errorf("nil limits: %v", err)
	}
}

// TestLimitsSchemaMatchesGo checks that taskspec.schema.json and the Task's schema agree with
// Limits.Validate on every case, and that the schemas reject an explicit cpus: 0 (which Go
// reads as unset).
func TestLimitsSchemaMatchesGo(t *testing.T) {
	schemas := []struct {
		name string
		doc  func(*testing.T, map[string]any) []byte
		sch  interface{ Validate(any) error }
	}{
		{"taskspec.schema.json", taskSpecWithLimits, compileSchemas(t)["taskspec.schema.json"]},
		{"Task", taskWithLimits, taskSchema(t)},
	}
	for _, s := range schemas {
		inst := func(limits map[string]any) any {
			var v map[string]any
			if err := json.Unmarshal(s.doc(t, limits), &v); err != nil {
				t.Fatal(err)
			}
			return schemaInstance(t, v)
		}
		for _, tt := range limitsCases {
			if schemaOK, goOK := s.sch.Validate(inst(tt.limits)) == nil, tt.want == nil; schemaOK != goOK {
				t.Errorf("%s %s: schema accepts = %v, Validate accepts = %v", s.name, tt.name, schemaOK, goOK)
			}
		}
		if s.sch.Validate(inst(map[string]any{"cpus": 0})) == nil {
			t.Errorf("%s: schema accepts cpus: 0", s.name)
		}
	}
}
