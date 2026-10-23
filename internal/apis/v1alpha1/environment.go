package v1alpha1

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// Limits of a workspace environment (P1 §4).
const (
	// MaxEnvScriptBytes is the most bytes each of setup, up and down may hold.
	MaxEnvScriptBytes = 64 << 10
	// MaxEnvReadyURLs is the most URLs ready.http may list.
	MaxEnvReadyURLs = 10
	// MaxEnvReadyTimeout is the longest ready.timeout.
	MaxEnvReadyTimeout = 60 * time.Minute
	// MaxEnvExposes is the most variables exposes may set.
	MaxEnvExposes = 20
	// MaxEnvExposeValueBytes is the most bytes an exposes value may hold.
	MaxEnvExposeValueBytes = 2048
	// MaxVersions is the most components a Task's spec.versions may list.
	MaxVersions = 50
	// MaxVersionBytes is the most bytes a spec.versions value may hold.
	MaxVersionBytes = 128
)

// Patterns of a workspace environment. The schema uses the same ones.
const (
	// EnvExposeNamePattern is what an exposes name must match.
	EnvExposeNamePattern = `^FACTORY_[A-Z0-9_]+$`
	// EnvVersionPrefix starts the variable the runtime exports for each spec.versions entry
	// (FACTORY_VERSION_<COMPONENT>); exposes may not use it.
	EnvVersionPrefix = "FACTORY_VERSION_"
	// VersionComponentPattern is what a spec.versions key must match.
	VersionComponentPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	// readyURLPattern is an absolute http or https URL with a host and no whitespace;
	// validateReady also parses it.
	readyURLPattern = `^https?://[^\s/?#]+([/?#]\S*)?$`
)

// ReservedEnvNames are the FACTORY_* variables the agent runtime sets in a run's environment
// (internal/agent: pipeline, gitcred, effector and stack). The runtime owns them, so an
// environment's exposes may not set them, nor any EnvVersionPrefix name. FACTORY_STACK_URL
// and FACTORY_STACK_API are not reserved: P1 §4 moves them from the stack to exposes.
var ReservedEnvNames = []string{
	"FACTORY_API",
	"FACTORY_EFFECTOR",
	"FACTORY_GIT_CREDENTIAL_SOCKET",
	"FACTORY_HARNESS",
	"FACTORY_HEADLESS",
	"FACTORY_OUT",
	"FACTORY_RESULT",
	"FACTORY_STACK_DIR",
	"FACTORY_STACK_PROJECT",
	"FACTORY_STACK_VERSIONS",
	"FACTORY_TASK_ID",
	"FACTORY_TOKEN",
}

// Environment validation errors. Workspace.Validate and Task.Validate return a
// *ValidationError wrapping one of them (or a host error, for egress.allow).
var (
	ErrEnvVersionsFrom   = errors.New("invalid versions.from")
	ErrEnvVersionsInput  = errors.New("invalid versions.input")
	ErrEnvScriptTooLong  = errors.New("script too long")
	ErrEnvNoUp           = errors.New("needs up")
	ErrEnvReadyURL       = errors.New("invalid ready URL")
	ErrEnvReadyTimeout   = errors.New("invalid ready timeout")
	ErrEnvExposeName     = errors.New("invalid exposes name")
	ErrEnvTooManyExposes = errors.New("too many exposes")
	ErrEnvExposeValue    = errors.New("invalid exposes value")
	ErrEnvEgressMode     = errors.New("invalid egress mode")
	ErrEnvEgressAllow    = errors.New("egress.allow needs deny-by-default")
	ErrEnvVersions       = errors.New("invalid versions")
)

var (
	exposeNameRE = regexp.MustCompile(EnvExposeNamePattern)
	componentRE  = regexp.MustCompile(VersionComponentPattern)
	readyURLRE   = regexp.MustCompile(readyURLPattern)
)

// Validate checks the environment's rules. Field paths are relative to the environment
// (versions.from, ready.http[0]). TaskSpec.Validate also checks that versions.input names one
// of the spec's inputs.
func (e *Environment) Validate() error {
	if v := e.Versions; v != nil {
		if err := v.validate(); err != nil {
			return prefixField("versions", err)
		}
	}
	for _, s := range []struct{ field, script string }{{"setup", e.Setup}, {"up", e.Up}, {"down", e.Down}} {
		if len(s.script) > MaxEnvScriptBytes {
			return &ValidationError{
				Field:   s.field,
				Message: fmt.Sprintf("%d bytes: at most %d", len(s.script), MaxEnvScriptBytes),
				Err:     ErrEnvScriptTooLong,
			}
		}
	}
	if e.Up == "" {
		if e.Ready != nil {
			return &ValidationError{Field: "ready", Message: "ready needs up", Err: ErrEnvNoUp}
		}
		if len(e.Exposes) > 0 {
			return &ValidationError{Field: "exposes", Message: "exposes needs up", Err: ErrEnvNoUp}
		}
	}
	if r := e.Ready; r != nil {
		if err := r.validate(); err != nil {
			return prefixField("ready", err)
		}
	}
	if err := validateExposes(e.Exposes); err != nil {
		return err
	}
	if eg := e.Egress; eg != nil {
		if err := eg.validate(); err != nil {
			return prefixField("egress", err)
		}
	}
	return nil
}

func (v *VersionSource) validate() error {
	switch v.From {
	case "latest":
		if v.Input != "" {
			return &ValidationError{Field: "input", Message: "not allowed with from: latest", Err: ErrEnvVersionsInput}
		}
	case "inputs", "train":
		if v.Input == "" {
			return &ValidationError{Field: "input", Message: "required with from: " + v.From, Err: ErrEnvVersionsInput}
		}
	default:
		return &ValidationError{Field: "from", Message: fmt.Sprintf("%q: want latest, inputs or train", v.From), Err: ErrEnvVersionsFrom}
	}
	return nil
}

func (r *ReadyCheck) validate() error {
	if len(r.HTTP) == 0 || len(r.HTTP) > MaxEnvReadyURLs {
		return &ValidationError{
			Field:   "http",
			Message: fmt.Sprintf("%d URLs: want 1 to %d", len(r.HTTP), MaxEnvReadyURLs),
			Err:     ErrEnvReadyURL,
		}
	}
	for i, raw := range r.HTTP {
		if err := validateReadyURL(raw); err != nil {
			return &ValidationError{Field: fmt.Sprintf("http[%d]", i), Message: err.Error(), Err: ErrEnvReadyURL}
		}
	}
	if r.Timeout != "" {
		d, err := r.Timeout.Parse()
		switch {
		case err != nil:
			return &ValidationError{Field: "timeout", Message: err.Error(), Err: ErrEnvReadyTimeout}
		case d <= 0 || d > MaxEnvReadyTimeout:
			return &ValidationError{
				Field:   "timeout",
				Message: fmt.Sprintf("%s: want more than 0 and at most %s", r.Timeout, MaxEnvReadyTimeout),
				Err:     ErrEnvReadyTimeout,
			}
		}
	}
	return nil
}

func validateReadyURL(raw string) error {
	if !readyURLRE.MatchString(raw) {
		return fmt.Errorf("%q: want an absolute http or https URL", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q: %w", raw, err)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q: no host", raw)
	}
	return nil
}

func validateExposes(exposes map[string]string) error {
	if len(exposes) > MaxEnvExposes {
		return &ValidationError{
			Field:   "exposes",
			Message: fmt.Sprintf("%d variables: at most %d", len(exposes), MaxEnvExposes),
			Err:     ErrEnvTooManyExposes,
		}
	}
	for _, name := range sortedKeys(exposes) {
		field := "exposes." + name
		switch {
		case !exposeNameRE.MatchString(name):
			return &ValidationError{Field: field, Message: "the name must match " + EnvExposeNamePattern, Err: ErrEnvExposeName}
		case slices.Contains(ReservedEnvNames, name) || strings.HasPrefix(name, EnvVersionPrefix):
			return &ValidationError{Field: field, Message: "the runtime sets " + name, Err: ErrEnvExposeName}
		}
		if v := exposes[name]; v == "" || len(v) > MaxEnvExposeValueBytes {
			return &ValidationError{
				Field:   field,
				Message: fmt.Sprintf("%d bytes: want 1 to %d", len(v), MaxEnvExposeValueBytes),
				Err:     ErrEnvExposeValue,
			}
		}
	}
	return nil
}

func (eg *EgressPolicy) validate() error {
	switch eg.Mode {
	case "", "default":
		if len(eg.Allow) > 0 {
			return &ValidationError{Field: "allow", Message: "only valid with mode: deny-by-default", Err: ErrEnvEgressAllow}
		}
		return nil
	case "deny-by-default":
		return validateHostList("allow", eg.Allow)
	default:
		return &ValidationError{Field: "mode", Message: fmt.Sprintf("%q: want default or deny-by-default", eg.Mode), Err: ErrEnvEgressMode}
	}
}

// validateVersions checks a Task's spec.versions. Field paths are relative to the Task's spec.
func validateVersions(versions map[string]string) error {
	if len(versions) > MaxVersions {
		return &ValidationError{
			Field:   "versions",
			Message: fmt.Sprintf("%d components: at most %d", len(versions), MaxVersions),
			Err:     ErrEnvVersions,
		}
	}
	for _, c := range sortedKeys(versions) {
		field := "versions." + c
		if !componentRE.MatchString(c) {
			return &ValidationError{Field: field, Message: "the component must match " + VersionComponentPattern, Err: ErrEnvVersions}
		}
		if v := versions[c]; v == "" || len(v) > MaxVersionBytes {
			return &ValidationError{
				Field:   field,
				Message: fmt.Sprintf("%d bytes: want 1 to %d", len(v), MaxVersionBytes),
				Err:     ErrEnvVersions,
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// JSONSchemaExtend constrains the scripts' length (in characters, so the byte limit only
// Validate checks) and exposes (names, reserved names, count, values).
func (Environment) JSONSchemaExtend(s *jsonschema.Schema) {
	for _, f := range []string{"setup", "up", "down"} {
		if p, ok := s.Properties.Get(f); ok {
			p.MaxLength = ptr(uint64(MaxEnvScriptBytes))
		}
	}
	p, ok := s.Properties.Get("exposes")
	if !ok {
		return
	}
	reserved := make([]any, len(ReservedEnvNames))
	for i, n := range ReservedEnvNames {
		reserved[i] = n
	}
	p.MaxProperties = ptr(uint64(MaxEnvExposes))
	p.PropertyNames = &jsonschema.Schema{
		Pattern: EnvExposeNamePattern,
		Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
			{Enum: reserved},
			{Pattern: "^" + EnvVersionPrefix},
		}},
	}
	p.AdditionalProperties = &jsonschema.Schema{
		Type:      "string",
		MinLength: ptr(uint64(1)),
		MaxLength: ptr(uint64(MaxEnvExposeValueBytes)),
	}
}

// JSONSchemaExtend constrains http: 1 to MaxEnvReadyURLs absolute http or https URLs. It
// stays optional in the schema (the field is omitzero); Validate requires it.
func (ReadyCheck) JSONSchemaExtend(s *jsonschema.Schema) {
	p, ok := s.Properties.Get("http")
	if !ok {
		return
	}
	p.MinItems = ptr(uint64(1))
	p.MaxItems = ptr(uint64(MaxEnvReadyURLs))
	p.Items = &jsonschema.Schema{Type: "string", Pattern: readyURLPattern}
}

// JSONSchemaExtend constrains allow like workspace.hosts (extendHostListSchema).
func (EgressPolicy) JSONSchemaExtend(s *jsonschema.Schema) { extendHostListSchema(s, "allow") }

// extendVersionsSchema constrains a Task's spec.versions the way validateVersions does.
func extendVersionsSchema(s *jsonschema.Schema) {
	p, ok := s.Properties.Get("versions")
	if !ok {
		return
	}
	p.MaxProperties = ptr(uint64(MaxVersions))
	p.PropertyNames = &jsonschema.Schema{Pattern: VersionComponentPattern}
	p.AdditionalProperties = &jsonschema.Schema{
		Type:      "string",
		MinLength: ptr(uint64(1)),
		MaxLength: ptr(uint64(MaxVersionBytes)),
	}
}
