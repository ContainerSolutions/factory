package catalog

import (
	"cmp"
	"fmt"
	"slices"
)

// Severity says whether a diagnostic fails the catalog.
type Severity string

// Severities. Warnings never fail a load unless Options.Strict is set.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Codes are stable: tools, fixtures and the UI match on them. The LLD's table
// (design/workstreams/dispatcher-core/LLD/catalog.md §Validation phases) gives the ranges; the
// meaning of each code within its range is fixed here.
const (
	// Loading.
	CodeDuplicate    = "CAT001" // two objects with the same (kind, name)
	CodeNotAnObject  = "CAT002" // a document without kind, or a kind that isn't a catalog kind
	CodeSyntax       = "CAT003" // a file that can't be read or isn't valid YAML
	CodeInvalidAsset = "CAT004" // a prompt or Template body that isn't a valid text/template, or a JSON Schema that doesn't compile

	// Phase 1, schema.
	CodeSchema = "CAT100" // the document doesn't match its kind's JSON Schema, or doesn't decode strictly

	// Phase 2, references.
	CodeUnknownTaskSpec = "CAT200" // dispatch/cancel names a TaskSpec that doesn't exist
	CodeUnknownQueue    = "CAT201" // dispatch names a Queue that doesn't exist
	CodeUnknownTemplate = "CAT202" // notify names a Template that doesn't exist
	CodeUnknownGroup    = "CAT203" // notify/trust/pool admission names a Group that doesn't exist
	CodeUnknownPool     = "CAT204" // a Queue names a Pool that doesn't exist
	CodeUnknownRule     = "CAT205" // RuleSet refs/exclude/overrides (or Rule spec.ruleSet) name a missing Rule (RuleSet)
	CodeUnknownProvider = "CAT206" // Group member, destination, target or pool backend names a Provider that doesn't exist
	CodeMissingPrompt   = "CAT207" // TaskSpec instructions.template isn't a file under prompts/
	CodeMissingSchema   = "CAT208" // TaskSpec output.schema isn't a file under schemas/
	CodeMissingFile     = "CAT209" // another referenced file (a Template's Block Kit file) doesn't exist

	// Phase 3, selection.
	CodeNoProviders      = "CAT300" // a RuleSet's providerSelector matches no Provider
	CodeNoRules          = "CAT301" // a RuleSet's rules.selector matches no Rule
	CodeProviderMismatch = "CAT302" // a selected rule's providerSelector matches none of the RuleSet's providers
	CodeBadSelector      = "CAT303" // an invalid label selector or scope glob

	// Phase 4, event vocabulary.
	CodeNotEmittable    = "CAT400" // an `on` type isn't emitted by any of the rule's effective providers
	CodeFactoryReserved = "CAT401" // a catalog Provider named or typed "factory" (reserved for factory.* events)
	CodeSchedule        = "CAT402" // schedule without on: [factory.schedule.tick] (or the reverse), or a malformed cron

	// Phase 5, params.
	CodeUnknownOverride = "CAT500" // an override key isn't a declared param of the rule
	CodeUnusedDefault   = "CAT501" // a RuleSet default no selected rule declares (warning)
	CodePlaceholder     = "CAT502" // a {{ }} placeholder that isn't {{ params.<declared> }}, or a substituted value of the wrong type

	// Phase 6, CEL.
	CodeWhere      = "CAT600" // a where that doesn't compile to bool
	CodeExpr       = "CAT601" // a "=" value that doesn't compile
	CodeField      = "CAT602" // a selection outside the documented field allow-list
	CodeCost       = "CAT603" // an estimated cost over the evaluation limit
	CodeNowInWhere = "CAT604" // now in a where without a schedule (warning)

	// Phase 7, dispatch.
	CodeInputMissing = "CAT700" // a required TaskSpec input isn't provided
	CodeInputUnknown = "CAT701" // a dispatch input the TaskSpec doesn't declare
	CodeInputValue   = "CAT702" // a literal input (or input default) of the wrong type or outside its enum
	CodeRequires     = "CAT703" // an invalid dispatch.requires label

	// Phase 8, harness.
	CodePattern          = "CAT800" // a harness pattern that doesn't compile
	CodeNotSubsumed      = "CAT801" // a request that can never fit its TaskSpec's constraints (harness, known models or efforts)
	CodeEffort           = "CAT802" // an effort outside the normalised set, or one the harness can't map
	CodeCapability       = "CAT803" // a harness without capabilities, or missing a feature the TaskSpec needs
	CodeDuplicateHarness = "CAT804" // the same harness twice in a TaskSpec's harness.allowed
	CodeNoHarness        = "CAT805" // a TaskSpec with neither harness.allowed nor harness.default

	// Phase 9, effects.
	CodeUnknownOp     = "CAT900" // an unknown op in a TaskSpec's effects.allow
	CodeEffectAction  = "CAT901" // an effect action with an unknown op or invalid op fields
	CodeReviewOp      = "CAT902" // review.* ops in a TaskSpec whose identity isn't reviewer
	CodeGitOp         = "CAT903" // push-branch/stack.* ops in a TaskSpec without a writable workspace
	CodeUnsupportedOp = "CAT904" // requestApproval, accepted but unsupported in the PoC (Q18) (warning)

	// Phase 10, state.
	CodeStateKey  = "CAT1000" // a set key outside the RuleSet's stateKeys
	CodeStateRead = "CAT1001" // an item.state key read in a where outside the RuleSet's stateKeys (warning)

	// Phase 11, workspace.
	CodeWorkspaceRW   = "CAT1100" // more than one rw repo in a TaskSpec's workspace
	CodeWorkspaceExpr = "CAT1101" // a TaskSpec "=" field that uses something other than inputs/task, or an undeclared input

	// Phase 12, overlap.
	CodeOverlap = "CAT1200" // two RuleSets select the same rule for overlapping scope and providers (warning)
)

// ObjectKey identifies a catalog object. Inline rules are named "<ruleset>/<rule>".
type ObjectKey struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (k ObjectKey) String() string { return k.Kind + "/" + k.Name }

// SourcePos is where an object's document starts.
type SourcePos struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Error is one validation diagnostic. File is relative to the catalog root; Line is 1-based
// (0 when unknown). Message starts with the field path when there is one.
type Error struct {
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	File     string    `json:"file"`
	Line     int       `json:"line"`
	Object   ObjectKey `json:"object,omitzero"`
	Severity Severity  `json:"severity"`
}

// Error formats the diagnostic as `file:line: CODE message`, warnings as
// `file:line: CODE warning: message`.
func (e Error) Error() string {
	pos := e.File
	if e.Line > 0 {
		pos = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	if e.Severity == SeverityWarning {
		return fmt.Sprintf("%s: %s warning: %s", pos, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s %s", pos, e.Code, e.Message)
}

// HasErrors reports whether any diagnostic is an error (warnings don't count).
func HasErrors(errs []Error) bool {
	return slices.ContainsFunc(errs, func(e Error) bool { return e.Severity == SeverityError })
}

// sortErrors orders diagnostics by file, line, code and message, and drops exact duplicates
// (the same rule checked through several RuleSets reports its own problems once).
func sortErrors(errs []Error) []Error {
	slices.SortFunc(errs, func(a, b Error) int {
		return cmp.Or(
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Message, b.Message),
			cmp.Compare(a.Severity, b.Severity),
		)
	})
	return slices.CompactFunc(errs, func(a, b Error) bool {
		return a.File == b.File && a.Line == b.Line && a.Code == b.Code && a.Message == b.Message && a.Severity == b.Severity
	})
}
