// Package catalog loads a factory catalog (a directory of YAML objects plus prompt and schema
// assets) into an immutable, validated snapshot, as specified by
// design/workstreams/dispatcher-core/LLD/catalog.md.
//
// LoadDir reads only through an fs.FS and runs every validation phase, collecting all
// diagnostics rather than stopping at the first. The package is pure: no network, no
// processes, no clock. Loading from a git ref lives in the gitsource subpackage.
package catalog

import (
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/cel"
	"github.com/compliance-framework/factory/internal/patterns"
)

// Catalog is a validated catalog snapshot. It is immutable after LoadDir: the runtime swaps
// whole snapshots (atomic pointer) and never modifies one, so it is safe to share between
// goroutines.
type Catalog struct {
	// SHA is the git commit the catalog was loaded from, or "dir:<sha256>" over the sorted
	// file contents of a local directory.
	SHA       string
	Providers map[string]*v1alpha1.Provider
	Groups    map[string]*v1alpha1.Group
	Templates map[string]*v1alpha1.Template
	// Rules are the standalone rules by name, and the inline rules by "<ruleset>/<rule>".
	// They are as written: RuleSets hold the substituted, compiled versions.
	Rules     map[string]*v1alpha1.Rule
	RuleSets  map[string]*CompiledRuleSet
	TaskSpecs map[string]*CompiledTaskSpec
	Queues    map[string]*v1alpha1.Queue
	Pools     map[string]*v1alpha1.Pool
	// Programs holds every compiled "=" value of the catalog (rule actions after params
	// substitution, TaskSpec fields), keyed by CEL source, for cel.EvalTree. Where
	// expressions are on EffectiveRule.Where.
	Programs cel.ProgramIndex
	// Sources is where each object's document starts.
	Sources map[ObjectKey]SourcePos
	Assets  Assets
}

// Assets are the non-object files of a catalog, keyed by their path relative to the catalog
// root (as TaskSpecs reference them: "prompts/implement.md.tmpl").
type Assets struct {
	// Prompts are the files under prompts/, parsed as text/template.
	Prompts map[string]*Prompt
	// Schemas are the *.json files under schemas/, compiled as JSON Schemas (draft 2020-12
	// by default). They may $ref the v1alpha1 schemas by their $id, e.g. the base Result.
	Schemas map[string]*jsonschema.Schema
}

// Prompt is one prompt template.
type Prompt struct {
	Path string
	// Text is the template source; Digest is "sha256:<hex>" of it (Task.spec.prompt.digest).
	Text     string
	Digest   string
	Template *template.Template
}

// CompiledRuleSet is a RuleSet with its selection resolved.
type CompiledRuleSet struct {
	Spec *v1alpha1.RuleSet
	// Providers are the provider instances spec.providerSelector matches, sorted.
	Providers []string
	Scope     CompiledScope
	// Rules are the effective rules after selection, exclude, defaults and overrides,
	// ordered by priority (highest first), then name.
	Rules []*EffectiveRule
}

// EffectiveRule is a rule as one RuleSet runs it.
type EffectiveRule struct {
	// Name is the rule's name ("<ruleset>/<rule>" for an inline rule).
	Name string
	// Rule is the rule after {{ params }} substitution with this RuleSet's params.
	Rule *v1alpha1.Rule
	// Params are the rule's params ← the RuleSet's defaults ← the RuleSet's overrides[rule].
	Params map[string]any
	// Where is the compiled where; an empty where compiles to a program that is always true.
	Where   *cel.Program
	Actions []CompiledAction
	// Providers are the RuleSet's providers the rule's providerSelector matches, plus
	// v1alpha1.FactoryProvider when `on` has factory.* types; ["factory"] for a
	// factory-only rule.
	Providers []string
}

// CompiledAction is one action of an EffectiveRule.
type CompiledAction struct {
	// Type is the action's member: dispatch, set, notify, requestApproval, cancel, link or
	// effect.
	Type string
	// Action is the typed action, after params substitution.
	Action v1alpha1.Action
	// Tree is the action's member as a YAML tree (maps, lists, scalars), for cel.EvalTree
	// with Catalog.Programs; its "=" leaves are all compiled there.
	Tree any
	// Harness is the compiled dispatch.harness request, when it is a literal object.
	Harness *patterns.Request
}

// CompiledScope is a RuleSet scope with its globs compiled. An empty list matches anything.
type CompiledScope struct {
	Repos, Exclude, Channels, Branches []patterns.Pattern
}

// MatchRepo reports whether repo ("owner/name") is in scope: it matches Repos (or Repos is
// empty) and no Exclude glob.
func (s CompiledScope) MatchRepo(repo string) bool {
	return matchAny(s.Repos, repo, true) && !matchAny(s.Exclude, repo, false)
}

// MatchChannel reports whether a channel is in scope (Channels empty means any).
func (s CompiledScope) MatchChannel(channel string) bool { return matchAny(s.Channels, channel, true) }

// MatchBranch reports whether a branch is in scope (Branches empty means any).
func (s CompiledScope) MatchBranch(branch string) bool { return matchAny(s.Branches, branch, true) }

func matchAny(ps []patterns.Pattern, s string, empty bool) bool {
	if len(ps) == 0 {
		return empty
	}
	for _, p := range ps {
		if p.Match(s) {
			return true
		}
	}
	return false
}

// CompiledTaskSpec is a TaskSpec with its assets, expressions and harness policy compiled.
type CompiledTaskSpec struct {
	Spec *v1alpha1.TaskSpec
	// Digest is "sha256:<hex>" of the spec's canonical JSON (Task.spec.taskSpec.digest).
	Digest string
	// Prompt is instructions.template; OutputSchema is output.schema (nil when unset).
	Prompt       *Prompt
	OutputSchema *jsonschema.Schema
	// Expressions are the "=" fields of the spec by field path
	// ("spec.workspace.repos[0].ref"); their programs are also in Catalog.Programs.
	Expressions map[string]*cel.Program
	// Constraints is harness.allowed compiled or, when the TaskSpec has none, one
	// constraint derived from harness.default (its harness, model and effort patterns; an
	// unset one is "*"), for patterns.Resolve. Default is harness.default compiled (nil when
	// unset).
	Constraints []patterns.Constraint
	Default     *patterns.Request
}

// ValidateInputs checks fully evaluated dispatch inputs against the declared inputs: every
// key is declared, required inputs without a default are present, and values have the
// declared type and are in the enum. It is the "input schema" of the LLD.
func (t *CompiledTaskSpec) ValidateInputs(inputs map[string]any) []error {
	var out []error
	for _, p := range checkInputs(t.Spec.Spec.Inputs, inputs, false) {
		out = append(out, p)
	}
	return out
}

// Options configure a load.
type Options struct {
	// Capabilities are the harness adapters' capability descriptors. Nil means the embedded
	// defaults (DefaultCapabilities).
	Capabilities []v1alpha1.AdapterCapabilities
	// KnownModels are the model ids each harness is known to offer, by harness name, for
	// patterns.Subsumes. Nil means the embedded defaults (DefaultKnownModels).
	KnownModels map[string][]string
	// Strict turns warnings into errors.
	Strict bool
	// SHA, when set, is the snapshot's SHA instead of the directory hash (the git commit
	// for catalogs loaded from git).
	SHA string
}
