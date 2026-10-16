package v1alpha1

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const ruleDoc = `kind: Rule
metadata: { name: r }
spec:
  on: [issue.opened]
  actions:
    - set: { triage: running }
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
			contains: []string{"document[1] (line 9, kind Queue)", `unknown field "concurency"`},
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
			contains: []string{"document[0] (line 1", `line 3: mapping key "pool" already defined at line 3`},
		},
		{
			name:     "duplicate key in a free-form map",
			in:       ruleDoc + "  params:\n    queue: a\n    queue: b\n",
			contains: []string{`line 9: mapping key "queue" already defined at line 8`},
		},
		{
			name:     "non-string key",
			in:       ruleDoc + "  params: { 1: a }\n",
			contains: []string{`line 7: mapping key "1" (!!int) is not a string; quote it`},
		},
		{
			name:     "merge key",
			in:       "base: &b { pool: p }\nkind: Queue\nmetadata: { name: q }\nspec: { <<: *b }\n",
			contains: []string{"merge keys (<<) are not supported"},
		},
		{
			name:     "YAML syntax error",
			in:       ruleDoc + "---\nkind: Queue\nspec: [\n",
			contains: []string{"document[1] (kind ?)", "yaml:"},
		},
		{
			name:     "yes is a string, not a boolean, so a bool field rejects it",
			in:       "kind: Provider\nmetadata: { name: p }\nspec: { type: slack, trust: [{ tier: anyone, workspaceMember: yes }] }\n",
			contains: []string{"kind Provider", "cannot unmarshal string"},
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
			name:     "on given twice",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], \"on\": [pr.opened], actions: [{ set: {} }] }\n",
			contains: []string{`mapping key "on" already defined`},
		},
		{
			name:     "group member with two sources",
			in:       "kind: Group\nmetadata: { name: g }\nspec: { members: [{ provider: p, team: a/b }, { provider: p, team: a/b, users: [x] }] }\n",
			contains: []string{"kind Group", "spec.members[1]: member source sets [team users]"},
		},
		{
			name:     "group member with no source",
			in:       "kind: Group\nmetadata: { name: g }\nspec: { members: [{ provider: p }] }\n",
			contains: []string{"spec.members[0]: member source sets none of [team orgRole users userGroup]"},
		},
		{
			name:     "destination with two targets",
			in:       ruleDoc + "    - notify: { template: t, to: [{ item: all }, { group: maintainers, via: slack-ccf, item: primary }] }\n",
			contains: []string{"spec.actions[1].notify.to[1]: destination sets [group item]"},
		},
		{
			name:     "destination with no target",
			in:       ruleDoc + "    - notify: { template: t, to: [{ via: slack-ccf, as: dm }] }\n",
			contains: []string{"spec.actions[1].notify.to[0]: destination sets none of [group channel item]"},
		},
		{
			name:     "inline rule destination",
			in:       "kind: RuleSet\nmetadata: { name: rs }\nspec:\n  providerSelector: {}\n  rules: {}\n  inlineRules: [{ metadata: { name: r }, spec: { on: [pr.opened], actions: [{ notify: { template: t, to: [{}] } }] } }]\n",
			contains: []string{"spec.inlineRules[0].spec.actions[0].notify.to[0]"},
		},
		{
			name:     "template body object without blocks",
			in:       "kind: Template\nmetadata: { name: t }\nspec: { bodies: { slack: {} } }\n",
			contains: []string{"blocks must not be empty"},
		},
		{
			name:     "empty expression",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ dispatch: { taskSpec: t, queue: q, harness: \"\" } }] }\n",
			contains: []string{`"" is not an expression`},
		},
		{
			name:     "bare value where a structure or an expression is expected",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ dispatch: { taskSpec: t, queue: q, harness: codex } }] }\n",
			contains: []string{`"codex" is not an expression`, `"{{ params.x }}" template`, `start with "=" (CEL)`, "write the value as an object"},
		},
		{
			name:     "effect args under args",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ effect: { op: label.add, args: { labels: [x] } } }] }\n",
			contains: []string{`not under "args"`},
		},
		{
			name:     "effect without op",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ effect: { target: primary } }] }\n",
			contains: []string{"effect: op is required"},
		},
		{
			name:     "effect target not a string",
			in:       "kind: Rule\nmetadata: { name: r }\nspec: { on: [issue.opened], actions: [{ effect: { op: close, target: [a] } }] }\n",
			contains: []string{"effect: target"},
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

// TestYAML12 checks that documents are read as YAML 1.2: on/yes/no/off are strings, in typed
// fields and in free-form maps alike.
func TestYAML12(t *testing.T) {
	in := `kind: Rule
metadata: { name: r }
spec:
  on: [issue.opened]
  where: no
  params: { on: yes, off: n, when: 2026-10-15, enabled: true }
  actions:
    - set: { on: on }
...
---
kind: Queue
metadata: { name: q }
spec: { pool: yes, retry: { max: 1, on: [agent-lost] } }
...
`
	objs, err := Decode([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("decoded %d documents, want 2", len(objs))
	}
	r := objs[0].(*Rule)
	if len(r.Spec.On) != 1 || r.Spec.Where != "no" {
		t.Errorf("spec = %+v", r.Spec)
	}
	wantParams := map[string]any{"on": "yes", "off": "n", "when": "2026-10-15", "enabled": true}
	for k, want := range wantParams {
		if got := r.Spec.Params[k]; got != want {
			t.Errorf("params[%q] = %#v, want %#v", k, got, want)
		}
	}
	if got := r.Spec.Actions[0].Set["on"]; got != "on" {
		t.Errorf(`set["on"] = %#v, want "on"`, got)
	}
	q := objs[1].(*Queue)
	if q.Spec.Pool != "yes" || q.Spec.Retry == nil || len(q.Spec.Retry.On) != 1 {
		t.Errorf("queue spec = %+v", q.Spec)
	}

	out, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	// YAML 1.2 output: on/yes/no are plain strings; only values that would read back as
	// another type (a date here) are quoted.
	text := string(out)
	for _, want := range []string{"\n  on:\n", "\n    on: yes\n", "\n  where: no\n", "\n    when: \"2026-10-15\"\n", "\n        on: on\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("encoded output lacks %q:\n%s", want, text)
		}
	}
	back, err := DecodeOne(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.(*Rule).Spec.Params["on"]; got != "yes" {
		t.Errorf(`re-decoded params["on"] = %#v`, got)
	}
}

// effectRule wraps effect actions (YAML flow mappings) in a Rule.
func effectRule(effects ...string) string {
	out := "kind: Rule\nmetadata: { name: r }\nspec:\n  on: [issue.opened]\n  actions:\n"
	for _, e := range effects {
		out += "    - effect: " + e + "\n"
	}
	return out
}

func TestEffectActionInline(t *testing.T) {
	tests := []struct {
		name   string
		effect string
		want   EffectAction
		json   string
	}{
		{
			name:   "label.add (04-flows)",
			effect: "{ op: label.add, labels: [factory:quarantined] }",
			want:   EffectAction{Op: OpLabelAdd, Args: map[string]any{"labels": []any{"factory:quarantined"}}},
			json:   `{"op":"label.add","labels":["factory:quarantined"]}`,
		},
		{
			name:   "create-issue (04-flows)",
			effect: `{ op: create-issue, provider: "{{ params.issueProvider }}", repo: "{{ params.issueRepo }}", from: run.result.issueDraft }`,
			want: EffectAction{Op: OpCreateIssue, Args: map[string]any{
				"provider": "{{ params.issueProvider }}", "repo": "{{ params.issueRepo }}", "from": "run.result.issueDraft",
			}},
			json: `{"op":"create-issue","from":"run.result.issueDraft","provider":"{{ params.issueProvider }}","repo":"{{ params.issueRepo }}"}`,
		},
		{
			name:   "close with target (10-poc)",
			effect: "{ op: close, target: primary }",
			want:   EffectAction{Op: OpClose, Target: "primary"},
			json:   `{"op":"close","target":"primary"}`,
		},
	}
	schemas := compileSchemas(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(effectRule(tt.effect))
			obj, err := DecodeOne(in)
			if err != nil {
				t.Fatal(err)
			}
			got := obj.(*Rule).Spec.Actions[0].Effect
			if !reflect.DeepEqual(*got, tt.want) {
				t.Fatalf("decoded %#v, want %#v", *got, tt.want)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.json {
				t.Errorf("marshalled %s, want %s", b, tt.json)
			}
			out, err := Encode(obj)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(genericYAML(t, in), genericYAML(t, out)) {
				t.Errorf("round trip changed the document:\n%s", out)
			}
			if err := schemas["rule.schema.json"].Validate(schemaInstance(t, genericYAML(t, in))); err != nil {
				t.Errorf("schema rejects it: %v", err)
			}
		})
	}
	for _, args := range []string{"op", "target", "args"} {
		if _, err := json.Marshal(EffectAction{Op: OpClose, Args: map[string]any{args: "x"}}); err == nil {
			t.Errorf("marshalled an args key %q", args)
		}
	}
}
