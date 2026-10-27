package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/cel"
	"github.com/ContainerSolutions/factory/internal/patterns"
)

// ruleEntry is a standalone or inline rule.
type ruleEntry struct {
	// name is the catalog name: the rule's name, or "<ruleset>/<rule>" for an inline rule;
	// bare is the rule's own name.
	name, bare string
	rule       *v1alpha1.Rule
	doc        *document
	// base is where the rule's fields start in doc: empty for a Rule document,
	// spec.inlineRules[i] for an inline rule (whose fields are then metadata and spec).
	base     fieldPath
	inlineOf string
	where    *cel.Program
	// own is the rule with its own params (checked once, whatever selects it).
	own *view
}

func (r *ruleEntry) path(elems ...any) fieldPath { return r.base.at(elems...) }

// ruleSetEntry is a RuleSet and its resolved selection.
type ruleSetEntry struct {
	rs        *v1alpha1.RuleSet
	doc       *document
	providers []string
	scope     CompiledScope
	selected  []*ruleEntry // after exclude, by name
	views     []*view      // one per selected rule, same order
}

// taskSpecEntry is a TaskSpec being compiled.
type taskSpecEntry struct {
	ts       *v1alpha1.TaskSpec
	doc      *document
	compiled *CompiledTaskSpec
	// constraintsOK is false when an allowed pattern doesn't compile; subsumption checks
	// against the TaskSpec are then skipped.
	constraintsOK bool
}

// validator holds the indexed catalog during validation.
type validator struct {
	*loader
	env *cel.Env

	providers map[string]*document
	groups    map[string]*document
	templates map[string]*document
	queues    map[string]*document
	pools     map[string]*document
	taskSpecs map[string]*taskSpecEntry
	rules     map[string]*ruleEntry // standalone by name, inline by "<ruleset>/<rule>"
	ruleSets  map[string]*ruleSetEntry
	programs  cel.ProgramIndex

	ruleNames    []string // sorted
	ruleSetNames []string // sorted
}

// validate runs the phases after schema validation (which runs as documents load) and
// assembles the snapshot. The phases run in the LLD's order over the whole catalog; checks of
// a rule's actions run on its params-substituted views, so references in templated fields are
// checked with the values RuleSets give them.
func (l *loader) validate() *Catalog {
	v := &validator{
		loader:    l,
		providers: map[string]*document{},
		groups:    map[string]*document{},
		templates: map[string]*document{},
		queues:    map[string]*document{},
		pools:     map[string]*document{},
		taskSpecs: map[string]*taskSpecEntry{},
		rules:     map[string]*ruleEntry{},
		ruleSets:  map[string]*ruleSetEntry{},
		programs:  cel.ProgramIndex{},
	}
	env, err := cel.NewEnv()
	if err != nil {
		l.add(SeverityError, CodeWhere, ".", 0, ObjectKey{}, "CEL environment: %v", err)
		return &Catalog{}
	}
	v.env = env
	v.index()
	v.checkFormats()          // phase 1 (formats the schema can't express)
	v.checkReferences()       // phase 2
	v.resolveSelection()      // phase 3
	v.checkVocabulary()       // phase 4
	v.buildViews()            // phase 5
	v.checkActionReferences() // phase 2, on the substituted actions
	v.checkCEL()              // phase 6
	v.checkDispatch()         // phase 7
	v.checkHarness()          // phase 8
	v.checkEffects()          // phase 9
	v.checkState()            // phase 10
	v.checkWorkspace()        // phase 11
	v.checkOverlap()          // phase 12
	return v.assemble()
}

// index files every decoded object by kind and name (CAT001 for duplicates) and splits out
// inline rules.
func (v *validator) index() {
	seen := map[ObjectKey]*document{}
	for _, d := range v.docs {
		if prev, dup := seen[d.key]; dup {
			v.errorf(d, fieldPath{"metadata", "name"}, CodeDuplicate, "%s %q is already defined at %s:%d", d.key.Kind, d.key.Name, prev.file, prev.root.Line)
			continue
		}
		seen[d.key] = d
		switch o := d.obj.(type) {
		case *v1alpha1.Provider:
			v.providers[o.GetName()] = d
		case *v1alpha1.Group:
			v.groups[o.GetName()] = d
		case *v1alpha1.Template:
			v.templates[o.GetName()] = d
		case *v1alpha1.Queue:
			v.queues[o.GetName()] = d
		case *v1alpha1.Pool:
			v.pools[o.GetName()] = d
		case *v1alpha1.TaskSpec:
			v.taskSpecs[o.GetName()] = &taskSpecEntry{ts: o, doc: d, constraintsOK: true, compiled: &CompiledTaskSpec{
				Spec: o, Digest: digest(o.Spec), Expressions: map[string]*cel.Program{},
			}}
		case *v1alpha1.Rule:
			v.rules[o.GetName()] = &ruleEntry{name: o.GetName(), bare: o.GetName(), rule: o, doc: d}
		case *v1alpha1.RuleSet:
			v.ruleSets[o.GetName()] = &ruleSetEntry{rs: o, doc: d}
		}
	}
	// Inline rules, after the standalone ones so their qualified names never shadow them.
	for _, name := range sortedKeys(v.ruleSets) {
		rse := v.ruleSets[name]
		local := map[string]bool{}
		for i, ir := range rse.rs.Spec.InlineRules {
			p := fieldPath{"spec", "inlineRules"}.at(i)
			if local[ir.Metadata.Name] {
				v.errorf(rse.doc, p.at("metadata", "name"), CodeDuplicate, "inline rule %q is defined twice in RuleSet %s", ir.Metadata.Name, name)
				continue
			}
			local[ir.Metadata.Name] = true
			r := &v1alpha1.Rule{TypeMeta: v1alpha1.TypeMeta{APIVersion: rse.rs.APIVersion, Kind: v1alpha1.KindRule}, Metadata: ir.Metadata, Spec: ir.Spec}
			q := name + "/" + ir.Metadata.Name
			v.rules[q] = &ruleEntry{name: q, bare: ir.Metadata.Name, rule: r, doc: rse.doc, base: p, inlineOf: name}
		}
	}
	v.ruleNames = sortedKeys(v.rules)
	v.ruleSetNames = sortedKeys(v.ruleSets)
}

// checkFormats checks the durations (phase 1: JSON Schema can't express their format).
func (v *validator) checkFormats() {
	check := func(d *document, p fieldPath, dur v1alpha1.Duration) {
		if dur == "" {
			return
		}
		if _, err := dur.Parse(); err != nil {
			v.errorf(d, p, CodeSchema, "%v", err)
		}
	}
	for _, d := range v.docs {
		switch o := d.obj.(type) {
		case *v1alpha1.Provider:
			if o.Spec.Resync != nil {
				check(d, fieldPath{"spec", "resync", "every"}, o.Spec.Resync.Every)
			}
		case *v1alpha1.Group:
			if o.Spec.Cache != nil {
				check(d, fieldPath{"spec", "cache", "ttl"}, o.Spec.Cache.TTL)
			}
		case *v1alpha1.TaskSpec:
			if o.Spec.Limits != nil {
				check(d, fieldPath{"spec", "limits", "maxWallClock"}, o.Spec.Limits.MaxWallClock)
			}
		case *v1alpha1.Pool:
			if o.Spec.Lease != nil {
				check(d, fieldPath{"spec", "lease", "ttl"}, o.Spec.Lease.TTL)
				check(d, fieldPath{"spec", "lease", "heartbeat"}, o.Spec.Lease.Heartbeat)
			}
		}
	}
	for _, name := range v.ruleNames {
		r := v.rules[name]
		if r.rule.Spec.Debounce != nil {
			check(r.doc, r.path("spec", "debounce", "window"), r.rule.Spec.Debounce.Window)
		}
	}
}

// checkReferences is phase 2 for everything but rule actions (checkActionReferences).
func (v *validator) checkReferences() {
	for _, name := range sortedKeys(v.providers) {
		d := v.providers[name]
		p := d.obj.(*v1alpha1.Provider)
		for i, t := range p.Spec.Trust {
			if t.Group != "" && v.groups[t.Group] == nil {
				v.errorf(d, fieldPath{"spec", "trust"}.at(i, "group"), CodeUnknownGroup, "Group %q does not exist", t.Group)
			}
		}
	}
	for _, name := range sortedKeys(v.groups) {
		d := v.groups[name]
		g := d.obj.(*v1alpha1.Group)
		for i, m := range g.Spec.Members {
			if v.providers[m.Provider] == nil {
				v.errorf(d, fieldPath{"spec", "members"}.at(i, "provider"), CodeUnknownProvider, "Provider %q does not exist", m.Provider)
			}
		}
	}
	for _, name := range sortedKeys(v.queues) {
		d := v.queues[name]
		q := d.obj.(*v1alpha1.Queue)
		if v.pools[q.Spec.Pool] == nil {
			v.errorf(d, fieldPath{"spec", "pool"}, CodeUnknownPool, "Pool %q does not exist", q.Spec.Pool)
		}
	}
	for _, name := range sortedKeys(v.pools) {
		d := v.pools[name]
		p := d.obj.(*v1alpha1.Pool)
		if a := p.Spec.Admission; a != nil && v.groups[a.Owners] == nil {
			v.errorf(d, fieldPath{"spec", "admission", "owners"}, CodeUnknownGroup, "Group %q does not exist", a.Owners)
		}
		if b := p.Spec.Backend; b != nil && b.GitHubActions != nil && v.providers[b.GitHubActions.ProviderRef] == nil {
			v.errorf(d, fieldPath{"spec", "backend", "githubActions", "providerRef"}, CodeUnknownProvider, "Provider %q does not exist", b.GitHubActions.ProviderRef)
		}
	}
	for _, name := range sortedKeys(v.templates) {
		v.checkTemplate(v.templates[name])
	}
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		s := e.ts.Spec
		if s.Instructions != nil {
			e.compiled.Prompt = v.prompts[s.Instructions.Template]
		}
		if s.Output != nil && s.Output.Schema != "" && v.schemas[s.Output.Schema] != nil {
			doc, err := bundleSchema(v.schemaRaw, s.Output.Schema)
			if err != nil {
				v.errorf(e.doc, fieldPath{"spec", "output", "schema"}, CodeInvalidAsset, "%v", err)
			} else {
				e.compiled.OutputSchema = v.schemas[s.Output.Schema]
				e.compiled.OutputSchemaDocument = doc
			}
		}
		if s.Instructions != nil && !v.isAsset(promptsDir, s.Instructions.Template) {
			v.errorf(e.doc, fieldPath{"spec", "instructions", "template"}, CodeMissingPrompt, "prompt %q does not exist (prompts are files under %s/)", s.Instructions.Template, promptsDir)
		}
		if s.Output != nil && s.Output.Schema != "" && !v.isAsset(schemasDir, s.Output.Schema) {
			v.errorf(e.doc, fieldPath{"spec", "output", "schema"}, CodeMissingSchema, "schema %q does not exist (result schemas are JSON files under %s/)", s.Output.Schema, schemasDir)
		}
	}
	for _, name := range v.ruleNames {
		r := v.rules[name]
		if rs := r.rule.Spec.RuleSet; rs != "" && v.ruleSets[rs] == nil {
			v.errorf(r.doc, r.path("spec", "ruleSet"), CodeUnknownRule, "RuleSet %q does not exist", rs)
		}
	}
	for _, name := range v.ruleSetNames {
		rse := v.ruleSets[name]
		sel := rse.rs.Spec.Rules
		for i, ref := range sel.Refs {
			if v.rules[ref] == nil {
				v.errorf(rse.doc, fieldPath{"spec", "rules", "refs"}.at(i), CodeUnknownRule, "Rule %q does not exist", ref)
			}
		}
		for i, ex := range sel.Exclude {
			if v.rules[ex] == nil && v.rules[name+"/"+ex] == nil {
				v.errorf(rse.doc, fieldPath{"spec", "rules", "exclude"}.at(i), CodeUnknownRule, "Rule %q does not exist", ex)
			}
		}
		for _, rule := range sortedKeys(rse.rs.Spec.Overrides) {
			if v.rules[rule] == nil && v.rules[name+"/"+rule] == nil {
				v.errorf(rse.doc, fieldPath{"spec", "overrides", rule}, CodeUnknownRule, "Rule %q does not exist", rule)
			}
		}
	}
}

// isAsset reports whether p is a file under dir/ (CAT004 covers one that doesn't parse).
func (v *validator) isAsset(dir, p string) bool {
	return strings.HasPrefix(p, dir+"/") && v.files[p]
}

// checkTemplate parses a Template's bodies (CAT004) and checks its Block Kit files (CAT209).
func (v *validator) checkTemplate(d *document) {
	t := d.obj.(*v1alpha1.Template)
	for _, pt := range sortedKeys(t.Spec.Bodies) {
		b := t.Spec.Bodies[pt]
		p := fieldPath{"spec", "bodies", string(pt)}
		if b.Blocks != "" {
			if !v.files[b.Blocks] {
				v.errorf(d, p.at("blocks"), CodeMissingFile, "file %q does not exist", b.Blocks)
			}
			continue
		}
		if _, err := template.New(t.GetName()).Parse(b.Text); err != nil {
			v.errorf(d, p, CodeInvalidAsset, "body is not a valid text/template: %v", err)
		}
	}
}

// assemble builds the snapshot from the validated entries.
func (v *validator) assemble() *Catalog {
	c := &Catalog{
		Providers: map[string]*v1alpha1.Provider{},
		Groups:    map[string]*v1alpha1.Group{},
		Templates: map[string]*v1alpha1.Template{},
		Rules:     map[string]*v1alpha1.Rule{},
		RuleSets:  map[string]*CompiledRuleSet{},
		TaskSpecs: map[string]*CompiledTaskSpec{},
		Queues:    map[string]*v1alpha1.Queue{},
		Pools:     map[string]*v1alpha1.Pool{},
		Programs:  v.programs,
		Sources:   map[ObjectKey]SourcePos{},
		Assets:    Assets{Prompts: v.prompts, Schemas: v.schemas, SchemaSources: v.schemaRaw},
	}
	for n, d := range v.providers {
		c.Providers[n] = d.obj.(*v1alpha1.Provider)
	}
	for n, d := range v.groups {
		c.Groups[n] = d.obj.(*v1alpha1.Group)
	}
	for n, d := range v.templates {
		c.Templates[n] = d.obj.(*v1alpha1.Template)
	}
	for n, d := range v.queues {
		c.Queues[n] = d.obj.(*v1alpha1.Queue)
	}
	for n, d := range v.pools {
		c.Pools[n] = d.obj.(*v1alpha1.Pool)
	}
	for n, e := range v.taskSpecs {
		c.TaskSpecs[n] = e.compiled
	}
	for n, r := range v.rules {
		c.Rules[n] = r.rule
		c.Sources[ObjectKey{Kind: v1alpha1.KindRule, Name: n}] = SourcePos{File: r.doc.file, Line: r.doc.line(r.base)}
	}
	for _, d := range v.docs {
		if _, ok := c.Sources[d.key]; !ok {
			c.Sources[d.key] = SourcePos{File: d.file, Line: d.root.Line}
		}
	}
	for n, rse := range v.ruleSets {
		crs := &CompiledRuleSet{Spec: rse.rs, Providers: rse.providers, Scope: rse.scope}
		for _, vw := range rse.views {
			if vw.rule == nil {
				continue
			}
			crs.Rules = append(crs.Rules, &EffectiveRule{
				Name:      vw.entry.name,
				Rule:      vw.rule,
				Params:    vw.params,
				Where:     vw.entry.where,
				Actions:   vw.actions,
				Providers: vw.providers,
			})
		}
		sort.SliceStable(crs.Rules, func(i, j int) bool {
			a, b := crs.Rules[i], crs.Rules[j]
			if a.Rule.Spec.Priority != b.Rule.Spec.Priority {
				return a.Rule.Spec.Priority > b.Rule.Spec.Priority
			}
			return a.Name < b.Name
		})
		c.RuleSets[n] = crs
	}
	return c
}

// digest is "sha256:<hex>" of v's canonical JSON (encoding/json sorts map keys).
func digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toTree converts a typed value to its YAML tree (maps, lists, scalars), as cel.EvalTree
// walks it.
func toTree(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return tree, nil
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// names lists the keys of m for messages.
func names[V any](m map[string]V) string { return strings.Join(sortedKeys(m), ", ") }

// compilePatterns compiles glob lists (scope repos, effect labels); problems go to report.
func compilePatterns(globs []string, report func(i int, err error)) []patterns.Pattern {
	var out []patterns.Pattern
	for i, g := range globs {
		p, err := patterns.Compile(g)
		if err != nil {
			report(i, err)
			continue
		}
		out = append(out, p)
	}
	return out
}
