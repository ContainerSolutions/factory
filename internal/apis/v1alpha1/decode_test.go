package v1alpha1

import (
	"errors"
	"strings"
	"testing"
)

const ruleDoc = `kind: Rule
metadata: { name: r }
spec:
  on: [issue.opened]
  actions: [{ set: { triage: running } }]
`

func TestDecodeErrors(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		is       error    // errors.Is target, when set
		contains []string // substrings of the message
	}{
		{
			name:     "unknown top-level field",
			in:       "kind: Group\nmetadata: { name: g }\nspec: { members: [] }\nextra: 1\n",
			contains: []string{"document[0]", "kind Group", `unknown field "extra"`},
		},
		{
			name:     "unknown nested field",
			in:       "kind: Pool\nmetadata: { name: p }\nspec: { mode: registered, lease: { ttl: 2m, tll: 3m } }\n",
			contains: []string{"kind Pool", `unknown field "tll"`},
		},
		{
			name:     "unknown field inside a custom-decoded type",
			in:       "kind: Template\nmetadata: { name: t }\nspec: { bodies: { slack: { blocks: a.json, colour: red } } }\n",
			contains: []string{"kind Template", `unknown field "colour"`},
		},
		{
			name:     "unknown field inside a templated value",
			in:       "kind: Rule\nmetadata: { name: r }\nspec:\n  on: [issue.opened]\n  actions: [{ dispatch: { taskSpec: t, queue: q, harness: { name: codex, model: x } } }]\n",
			contains: []string{"kind Rule", `unknown field "model"`},
		},
		{
			name:     "unknown field in a rule spec",
			in:       ruleDoc + "  when: now\n",
			contains: []string{"kind Rule", `unknown field "when"`},
		},
		{
			name:     "error names the document index and line",
			in:       ruleDoc + "---\n# second\nkind: Queue\nmetadata: { name: q }\nspec: { pool: p, concurency: 2 }\n",
			contains: []string{"document[1] (line 7, kind Queue)", `unknown field "concurency"`},
		},
		{
			name:     "unknown kind",
			in:       "kind: Widget\nmetadata: { name: w }\n",
			is:       ErrUnknownKind,
			contains: []string{"kind Widget", `unknown kind "Widget"`},
		},
		{
			name:     "missing kind",
			in:       "metadata: { name: w }\n",
			is:       ErrMissingKind,
			contains: []string{"document[0]", "missing kind"},
		},
		{
			name:     "apiVersion mismatch",
			in:       "apiVersion: factory.ccf.dev/v1beta1\n" + ruleDoc,
			is:       ErrAPIVersion,
			contains: []string{"kind Rule", `"factory.ccf.dev/v1beta1"`},
		},
		{
			name:     "duplicate key",
			in:       "kind: Queue\nmetadata: { name: q }\nspec: { pool: p, pool: r }\n",
			contains: []string{"kind Queue", "pool"},
		},
		{
			name:     "action with no member",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{}] }\n",
			contains: []string{"spec.actions[0]", "no member"},
		},
		{
			name:     "action with two members",
			in:       "kind: Rule\nmetadata: { name: r }\nspec:\n  on: [issue.opened]\n  actions:\n    - set: { a: b }\n    - { set: { a: b }, cancel: { taskSpec: t } }\n",
			contains: []string{"spec.actions[1]", "2 members [set cancel]"},
		},
		{
			name:     "inline rule action with two members",
			in:       "kind: RuleSet\nmetadata: { name: rs }\nspec:\n  providerSelector: {}\n  rules: {}\n  inlineRules: [{ metadata: { name: r }, spec: { on: [pr.opened], actions: [{ set: {}, link: { from: x } }] } }]\n",
			contains: []string{"spec.inlineRules[0].spec.actions[0]", "2 members"},
		},
		{
			name:     "on given twice through YAML 1.1",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], \"on\": [pr.opened], actions: [{ set: {} }] }\n",
			contains: []string{`both "on"`},
		},
		{
			name:     "template body object without blocks",
			in:       "kind: Template\nmetadata: { name: t }\nspec: { bodies: { slack: {} } }\n",
			contains: []string{"blocks must not be empty"},
		},
		{
			name:     "empty expression",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ dispatch: { taskSpec: t, queue: q, harness: \"\" } }] }\n",
			contains: []string{"expression must not be empty"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
			if err == nil {
				t.Fatal("want an error")
			}
			var de *DocumentError
			if !errors.As(err, &de) {
				t.Fatalf("want a *DocumentError, got %T: %v", err, err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("want errors.Is(err, %v): %v", tt.is, err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q doesn't contain %q", err, s)
				}
			}
		})
	}
}

func TestActionValidationError(t *testing.T) {
	_, err := Decode([]byte("kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{}] }\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a *ValidationError, got %v", err)
	}
	if ve.Field != "spec.actions[0]" {
		t.Errorf("field = %q", ve.Field)
	}
}

func TestActionType(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   string
		ok     bool
	}{
		{"dispatch", Action{Dispatch: &DispatchAction{TaskSpec: "t", Queue: "q"}}, "dispatch", true},
		{"empty set counts", Action{Set: map[string]any{}}, "set", true},
		{"notify", Action{Notify: &NotifyAction{}}, "notify", true},
		{"requestApproval", Action{RequestApproval: &ApprovalSpec{}}, "requestApproval", true},
		{"cancel", Action{Cancel: &CancelAction{}}, "cancel", true},
		{"link", Action{Link: &LinkAction{}}, "link", true},
		{"effect", Action{Effect: &EffectAction{Op: OpClose}}, "effect", true},
		{"none", Action{}, "", false},
		{"two", Action{Cancel: &CancelAction{}, Effect: &EffectAction{}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.action.Type(); got != tt.want {
				t.Errorf("Type() = %q, want %q", got, tt.want)
			}
			if err := tt.action.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestDecodeMultiDocument(t *testing.T) {
	in := "---\n# leading separator and comment-only documents are skipped\n---\n" +
		"apiVersion: " + APIVersion + "\n" + ruleDoc +
		"--- # a comment on the separator\n" +
		"kind: Queue\nmetadata: { name: q }\nspec: { pool: p }\n" +
		"---\r\n" +
		"kind: Group\r\nmetadata: { name: g }\r\nspec: { members: [{ provider: github-public, users: [a] }] }\r\n" +
		"---\n\n"
	objs, err := Decode([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.GetTypeMeta().Kind+"/"+o.GetName())
	}
	if got, want := strings.Join(kinds, ","), "Rule/r,Queue/q,Group/g"; got != want {
		t.Fatalf("decoded %s, want %s", got, want)
	}
	r := objs[0].(*Rule)
	if r.APIVersion != APIVersion || len(r.Spec.On) != 1 || r.Spec.On[0] != EventIssueOpened {
		t.Errorf("rule decoded as %+v", r)
	}
	if _, err := DecodeOne([]byte(in)); err == nil {
		t.Error("DecodeOne accepted three documents")
	}
}

func TestEncodeKeepsAPIVersionAsWritten(t *testing.T) {
	for _, tt := range []struct {
		name, in string
		want     bool
	}{
		{"omitted", ruleDoc, false},
		{"present", "apiVersion: " + APIVersion + "\n" + ruleDoc, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj, err := DecodeOne([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			out, err := Encode(obj)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(out), "apiVersion"); got != tt.want {
				t.Errorf("apiVersion in output = %v, want %v:\n%s", got, tt.want, out)
			}
		})
	}
}

func TestNewEveryKind(t *testing.T) {
	for _, kind := range append(ConfigKinds(), RuntimeKinds()...) {
		obj, err := New(kind)
		if err != nil {
			t.Fatalf("New(%s): %v", kind, err)
		}
		if obj.GetTypeMeta().Kind != kind {
			t.Errorf("New(%s) has kind %q", kind, obj.GetTypeMeta().Kind)
		}
	}
	if IsConfigKind(KindTask) || !IsConfigKind(KindTaskSpec) {
		t.Error("IsConfigKind is wrong")
	}
}
