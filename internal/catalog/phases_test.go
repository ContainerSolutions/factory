package catalog

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// base is a small catalog that validates with no diagnostics; each phase fixture changes it.
var base = map[string]string{
	"providers/gh.yaml": `kind: Provider
metadata: { name: gh, labels: { family: github } }
spec:
  type: github
  github:
    webhook: { path: /hooks/gh, secretRef: { name: s, key: k } }
    identities:
      author: { appID: 1, privateKeyRef: { name: a, key: k } }
  trust:
    - { tier: maintainer, group: maintainers }
---
kind: Group
metadata: { name: maintainers }
spec:
  members: [{ provider: gh, users: [octocat] }]
`,
	"pools/local.yaml": `kind: Pool
metadata: { name: local }
spec: { mode: registered, admission: { tokenRef: { name: t, key: k }, owners: maintainers }, lease: { ttl: 2m } }
`,
	"queues/q.yaml": `kind: Queue
metadata: { name: q }
spec: { pool: local }
`,
	"templates/hello.yaml": `kind: Template
metadata: { name: hello }
spec: { bodies: { github: "hi {{ .item.id }}" } }
`,
	"taskspecs/work.yaml": `kind: TaskSpec
metadata: { name: work }
spec:
  inputs:
    target: { type: string, enum: [pr, stack], default: pr }
    issue:  { type: string, required: true }
  instructions: { template: prompts/work.md.tmpl }
  harness:
    allowed: [{ name: codex, models: ["gpt-5*"], reasoningEffort: [low, medium, high] }]
    default: { name: codex, config: { model: "gpt-5*", reasoningEffort: medium } }
  workspace: { repos: [{ ref: "=task.target.repo + '@main'", mode: rw }] }
  identity: author
  effects: { allow: [push-branch, pr.open] }
  output: { schema: schemas/work.json, outcomes: [done] }
`,
	"prompts/work.md.tmpl": "Work on {{ .inputs.issue }}.\n",
	"schemas/work.json":    `{"allOf": [{"$ref": "https://factory.ccf.dev/schemas/v1alpha1/result.schema.json"}]}`,
	"rules/work.yaml": `kind: Rule
metadata: { name: work-on-issue, labels: { pack: p } }
spec:
  providerSelector: { matchLabels: { family: github } }
  on: [issue.opened]
  where: "actor.tier == 'maintainer'"
  params: { queue: q }
  actions:
    - dispatch: { taskSpec: work, queue: "{{ params.queue }}", inputs: { issue: "=event.object.ref" } }
    - set: { work: started }
    - notify: { template: hello, to: [{ item: primary }] }
`,
	"rulesets/rs.yaml": `kind: RuleSet
metadata: { name: rs }
spec:
  providerSelector: { matchLabels: { family: github } }
  scope: { repos: ["org/*"] }
  rules: { selector: { matchLabels: { pack: p } } }
  stateKeys: [work]
`,
}

// rule is the base rule with its spec lines replaced.
func rule(spec string) string {
	return "kind: Rule\nmetadata: { name: work-on-issue, labels: { pack: p } }\nspec:\n" + spec
}

const ruleHead = `  providerSelector: { matchLabels: { family: github } }
  on: [issue.opened]
`

func dispatchRule(dispatch string) string {
	return rule(ruleHead + "  actions:\n    - dispatch: " + dispatch + "\n")
}

// fsWith returns base with files replaced, added, or removed (empty content).
func fsWith(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for p, s := range base {
		out[p] = &fstest.MapFile{Data: []byte(s)}
	}
	for p, s := range files {
		if s == "" {
			delete(out, p)
			continue
		}
		out[p] = &fstest.MapFile{Data: []byte(s)}
	}
	return out
}

func format(errs []Error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error())
		b.WriteByte('\n')
	}
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (run go test -update after checking it):\n--- got\n%s--- want\n%s", path, got, want)
	}
}

func TestBaseIsValid(t *testing.T) {
	c, errs := LoadDir(fsWith(nil), Options{})
	if len(errs) > 0 || c == nil {
		t.Fatalf("base catalog: %d diagnostics:\n%s", len(errs), format(errs))
	}
}

// TestPhases has one failing fixture per code, each with its golden output.
func TestPhases(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		files map[string]string
	}{
		// Loading.
		{"duplicate", CodeDuplicate, map[string]string{"rules/again.yaml": base["rules/work.yaml"]}},
		{"no-kind", CodeNotAnObject, map[string]string{"rules/x.yaml": "metadata: { name: x }\nspec: {}\n"}},
		{"runtime-kind", CodeNotAnObject, map[string]string{"rules/x.yaml": "kind: Event\nid: e1\n"}},
		{"yaml-syntax", CodeSyntax, map[string]string{"rules/x.yaml": "kind: Rule\nmetadata: { name: [\n"}},
		{"bad-prompt", CodeInvalidAsset, map[string]string{"prompts/work.md.tmpl": "line one\nWork on {{ .inputs.issue\n"}},
		{"bad-result-schema", CodeInvalidAsset, map[string]string{"schemas/work.json": `{"type": 12}`}},
		{"bad-template-body", CodeInvalidAsset, map[string]string{"templates/hello.yaml": "kind: Template\nmetadata: { name: hello }\nspec: { bodies: { github: \"hi {{ .item.id \" } }\n"}},

		// Phase 1.
		{"schema-unknown-field", CodeSchema, map[string]string{"rules/work.yaml": rule(ruleHead + "  when: x\n  actions: [{ set: { work: a } }]\n")}},
		{"schema-enum", CodeSchema, map[string]string{"pools/local.yaml": "kind: Pool\nmetadata: { name: local }\nspec: { mode: lent }\n"}},
		{"schema-duration", CodeSchema, map[string]string{"pools/local.yaml": "kind: Pool\nmetadata: { name: local }\nspec: { mode: registered, lease: { ttl: 5 minutes } }\n"}},
		{"schema-second-document", CodeSchema, map[string]string{"queues/q.yaml": base["queues/q.yaml"] + "---\nkind: Queue\nmetadata: { name: q2 }\nspec: { pool: local, concurrency: lots }\n"}},

		// Phase 2.
		{"unknown-taskspec", CodeUnknownTaskSpec, map[string]string{"rules/work.yaml": rule(ruleHead + "  actions: [{ cancel: { taskSpec: nope } }]\n")}},
		{"unknown-queue", CodeUnknownQueue, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "queue: q }", "queue: nope }", 1)}},
		{"unknown-template", CodeUnknownTemplate, map[string]string{"rules/work.yaml": rule(ruleHead + "  actions: [{ notify: { template: nope, to: [{ item: primary }] } }]\n")}},
		{"unknown-group", CodeUnknownGroup, map[string]string{"rules/work.yaml": rule(ruleHead + "  actions: [{ notify: { template: hello, to: [{ group: nope }] } }]\n")}},
		{"unknown-pool", CodeUnknownPool, map[string]string{"queues/q.yaml": "kind: Queue\nmetadata: { name: q }\nspec: { pool: nope }\n"}},
		{"unknown-rule", CodeUnknownRule, map[string]string{"rulesets/rs.yaml": strings.Replace(base["rulesets/rs.yaml"], "rules: {", "rules: { refs: [nope], exclude: [gone],", 1)}},
		{"unknown-provider", CodeUnknownProvider, map[string]string{"providers/gh.yaml": strings.Replace(base["providers/gh.yaml"], "[{ provider: gh,", "[{ provider: gitlab,", 1)}},
		{"missing-prompt", CodeMissingPrompt, map[string]string{"prompts/work.md.tmpl": ""}},
		{"missing-schema", CodeMissingSchema, map[string]string{"schemas/work.json": ""}},
		{"missing-blocks-file", CodeMissingFile, map[string]string{"templates/hello.yaml": "kind: Template\nmetadata: { name: hello }\nspec:\n  bodies:\n    github: hi\n    slack: { blocks: templates/hello.slack.json }\n"}},

		// Phase 3.
		{"no-providers", CodeNoProviders, map[string]string{"rulesets/rs.yaml": strings.Replace(base["rulesets/rs.yaml"], "providerSelector: { matchLabels: { family: github } }", "providerSelector: { matchLabels: { family: slack } }", 1)}},
		{"no-rules", CodeNoRules, map[string]string{"rulesets/rs.yaml": strings.Replace(base["rulesets/rs.yaml"], "pack: p", "pack: none", 1)}},
		{"provider-mismatch", CodeProviderMismatch, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "family: github", "family: gitlab", 1)}},
		{"missing-provider-selector", CodeProviderMismatch, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "  providerSelector: { matchLabels: { family: github } }\n", "", 1)}},
		{"bad-scope-glob", CodeBadSelector, map[string]string{"rulesets/rs.yaml": strings.Replace(base["rulesets/rs.yaml"], `["org/*"]`, `["org/{a"]`, 1)}},
		{"bad-selector", CodeBadSelector, map[string]string{"rulesets/rs.yaml": strings.Replace(base["rulesets/rs.yaml"], "{ matchLabels: { pack: p } }", "{ matchLabels: { pack: p }, matchExpressions: [{ key: pack, operator: In }] }", 1)}},

		// Phase 4.
		{"not-emittable", CodeNotEmittable, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "on: [issue.opened]", "on: [issue.opened, message.posted]", 1)}},
		{"factory-provider", CodeFactoryReserved, map[string]string{"providers/factory.yaml": strings.Replace(strings.Split(base["providers/gh.yaml"], "---")[0], "name: gh, labels: { family: github }", "name: factory", 1)}},
		{"schedule-without-tick", CodeSchedule, map[string]string{"rules/work.yaml": rule(ruleHead + "  schedule: \"0 * * * *\"\n  actions: [{ set: { work: a } }]\n")}},
		{"tick-without-schedule", CodeSchedule, map[string]string{"rules/tick.yaml": "kind: Rule\nmetadata: { name: tick, labels: { pack: p } }\nspec:\n  on: [factory.schedule.tick]\n  actions: [{ set: { work: a } }]\n"}},
		{"bad-cron", CodeSchedule, map[string]string{"rules/tick.yaml": "kind: Rule\nmetadata: { name: tick, labels: { pack: p } }\nspec:\n  on: [factory.schedule.tick]\n  schedule: \"0 * *\"\n  actions: [{ set: { work: a } }]\n"}},

		// Phase 5.
		{"unknown-override", CodeUnknownOverride, map[string]string{"rulesets/rs.yaml": base["rulesets/rs.yaml"] + "  overrides:\n    work-on-issue: { queue: q, nope: 1 }\n"}},
		{"unused-default", CodeUnusedDefault, map[string]string{"rulesets/rs.yaml": base["rulesets/rs.yaml"] + "  defaults: { nope: 1 }\n"}},
		{"unknown-placeholder", CodePlaceholder, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "{{ params.queue }}", "{{ params.missing }}", 1)}},
		{"override-breaks-substitution", CodeUnknownQueue, map[string]string{"rulesets/rs.yaml": base["rulesets/rs.yaml"] + "  overrides:\n    work-on-issue: { queue: elsewhere }\n"}},

		// Phase 6.
		{"where-syntax", CodeWhere, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"actor.tier =="`, 1)}},
		{"where-not-bool", CodeWhere, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"size(event.labels)"`, 1)}},
		{"value-syntax", CodeExpr, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"=event.object.ref"`, `"=event.object.ref +"`, 1)}},
		{"unknown-field", CodeField, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"event.release.tagg == 'v1'"`, 1)}},
		{"cost", CodeCost, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"event.labels.all(a, event.labels.all(b, event.labels.all(c, a == b && b == c)))"`, 1)}},
		{"now-without-schedule", CodeNowInWhere, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"now > timestamp('2026-01-01T00:00:00Z')"`, 1)}},

		// Phase 7.
		{"input-missing", CodeInputMissing, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { target: pr } }")}},
		{"input-unknown", CodeInputUnknown, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: \"=event.object.ref\", mode: new } }")}},
		{"input-value", CodeInputValue, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: 42, target: tree } }")}},
		{"input-default", CodeInputValue, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "default: pr }", "default: tree }", 1)}},
		{"requires-label", CodeRequires, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: x }, requires: { labels: [\"has local stack\"] } }")}},

		// Phase 8.
		{"bad-pattern", CodePattern, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], `models: ["gpt-5*"]`, `models: ["re:gpt-(5"]`, 1)}},
		{"not-subsumed-model", CodeNotSubsumed, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: x }, harness: { name: codex, config: { model: \"claude-*\" } } }")}},
		{"not-subsumed-harness", CodeNotSubsumed, map[string]string{"rules/work.yaml": dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: x }, harness: { name: claude-code } }")}},
		{"not-subsumed-effort", CodeNotSubsumed, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "reasoningEffort: medium }", "reasoningEffort: max }", 1)}},
		{"effort", CodeEffort, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "[low, medium, high]", "[low, medium, extreme]", 1)}},
		{"effort-unmapped", CodeEffort, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "[low, medium, high]", "[minimal]", 1)}},
		{"no-capabilities", CodeCapability, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "allowed: [", "allowed: [{ name: aider }, ", 1)}},
		{"no-harness", CodeNoHarness, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "  harness:\n    allowed: [{ name: codex, models: [\"gpt-5*\"], reasoningEffort: [low, medium, high] }]\n    default: { name: codex, config: { model: \"gpt-5*\", reasoningEffort: medium } }\n", "", 1)}},
		{"not-subsumed-derived", CodeNotSubsumed, map[string]string{
			"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "    allowed: [{ name: codex, models: [\"gpt-5*\"], reasoningEffort: [low, medium, high] }]\n", "", 1),
			"rules/work.yaml":     dispatchRule("{ taskSpec: work, queue: q, inputs: { issue: x }, harness: { name: codex, config: { model: \"gpt-5*\", reasoningEffort: high } } }"),
		}},
		{"duplicate-harness", CodeDuplicateHarness, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "allowed: [", "allowed: [{ name: codex, models: \"*\" }, ", 1)}},

		// Phase 9.
		{"unknown-op", CodeUnknownOp, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "[push-branch, pr.open]", "[push-branch, force-push]", 1)}},
		{"effect-action", CodeEffectAction, map[string]string{"rules/work.yaml": rule(ruleHead + "  actions: [{ effect: { op: label.add, target: primary } }]\n")}},
		{"review-op", CodeReviewOp, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "[push-branch, pr.open]", "[push-branch, review.approve]", 1)}},
		{"git-op", CodeGitOp, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "mode: rw", "mode: ro", 1)}},
		{"request-approval", CodeUnsupportedOp, map[string]string{"rules/work.yaml": rule(ruleHead + "  actions: [{ requestApproval: { name: implement, approvers: maintainers } }]\n")}},

		// Phase 10.
		{"state-key", CodeStateKey, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], "set: { work: started }", "set: { work: started, other: 1 }", 1)}},
		{"state-read", CodeStateRead, map[string]string{"rules/work.yaml": strings.Replace(base["rules/work.yaml"], `"actor.tier == 'maintainer'"`, `"!has(item.state.work) && !has(item.state.other)"`, 1)}},

		// Phase 11.
		{"two-rw-repos", CodeWorkspaceRW, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "mode: rw }]", "mode: rw }, { ref: org/other@main, mode: rw }]", 1)}},
		{"workspace-expr", CodeWorkspaceExpr, map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], `"=task.target.repo + '@main'"`, `"=event.repo + '@' + inputs.branch"`, 1)}},

		// Phase 12.
		{"overlap", CodeOverlap, map[string]string{"rulesets/rs2.yaml": strings.Replace(base["rulesets/rs.yaml"], "name: rs }", "name: rs2 }", 1)}},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		seen[tt.code] = true
		t.Run(tt.name, func(t *testing.T) {
			c, errs := LoadDir(fsWith(tt.files), Options{})
			found := false
			for _, e := range errs {
				if e.Code == tt.code {
					found = true
				}
			}
			if !found {
				t.Errorf("want a %s diagnostic, got:\n%s", tt.code, format(errs))
			}
			if HasErrors(errs) != (c == nil) {
				t.Errorf("catalog returned = %v with errors = %v", c != nil, HasErrors(errs))
			}
			checkGolden(t, tt.name, format(errs))
		})
	}
	for _, code := range allCodes {
		if !seen[code] {
			t.Errorf("no fixture for %s", code)
		}
	}
}

// allCodes lists every code, so TestPhases fails when one has no fixture.
var allCodes = []string{
	CodeDuplicate, CodeNotAnObject, CodeSyntax, CodeInvalidAsset,
	CodeSchema,
	CodeUnknownTaskSpec, CodeUnknownQueue, CodeUnknownTemplate, CodeUnknownGroup, CodeUnknownPool,
	CodeUnknownRule, CodeUnknownProvider, CodeMissingPrompt, CodeMissingSchema, CodeMissingFile,
	CodeNoProviders, CodeNoRules, CodeProviderMismatch, CodeBadSelector,
	CodeNotEmittable, CodeFactoryReserved, CodeSchedule,
	CodeUnknownOverride, CodeUnusedDefault, CodePlaceholder,
	CodeWhere, CodeExpr, CodeField, CodeCost, CodeNowInWhere,
	CodeInputMissing, CodeInputUnknown, CodeInputValue, CodeRequires,
	CodePattern, CodeNotSubsumed, CodeEffort, CodeCapability, CodeDuplicateHarness, CodeNoHarness,
	CodeUnknownOp, CodeEffectAction, CodeReviewOp, CodeGitOp, CodeUnsupportedOp,
	CodeStateKey, CodeStateRead,
	CodeWorkspaceRW, CodeWorkspaceExpr,
	CodeOverlap,
}

func TestStrictTurnsWarningsIntoErrors(t *testing.T) {
	fsys := fsWith(map[string]string{"rulesets/rs.yaml": base["rulesets/rs.yaml"] + "  defaults: { nope: 1 }\n"})
	if c, errs := LoadDir(fsys, Options{}); c == nil || HasErrors(errs) || len(errs) != 1 {
		t.Fatalf("lenient: catalog %v, diagnostics:\n%s", c != nil, format(errs))
	}
	c, errs := LoadDir(fsys, Options{Strict: true})
	if c != nil || !HasErrors(errs) {
		t.Fatalf("strict: catalog %v, diagnostics:\n%s", c != nil, format(errs))
	}
}

// TestThreadOpsForAuthors: thread.reply and thread.resolve are allowed for both identities
// (the author addresses review feedback); only review.* needs the reviewer (CAT902).
func TestThreadOpsForAuthors(t *testing.T) {
	fsys := fsWith(map[string]string{"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "[push-branch, pr.open]", "[push-branch, pr.open, thread.reply, thread.resolve]", 1)})
	if c, errs := LoadDir(fsys, Options{}); c == nil || len(errs) > 0 {
		t.Errorf("author TaskSpec with thread ops:\n%s", format(errs))
	}
}
