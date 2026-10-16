package catalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// NormalisedEfforts are the reasoning efforts catalogs write; adapters map them to their
// harness's own values (AdapterCapabilities.EffortMapping). ADR-0003.
var NormalisedEfforts = []string{"minimal", "low", "medium", "high", "max"}

// Harness features a TaskSpec can need (AdapterCapabilities.Features).
const (
	FeatureStructuredOutput = "structured-output" // output.schema
	FeatureMCP              = "mcp"               // effects, through the Effector's MCP server
	FeaturePlugins          = "plugins"           // skills, as a plugin dir
	FeatureSkillsDir        = "skills-dir"        // skills, as a skills dir
)

// defaultHarnesses is the embedded default for `factory validate`: the codex and claude-code
// adapters with the models and efforts of the PoC agents (design/10-poc.md). The AGT
// adapters' static descriptors replace it once they exist (AGT-S1).
//
//go:embed defaults/harnesses.json
var defaultHarnesses []byte

// Harnesses is the file format of the embedded defaults: capability descriptors plus the
// known model ids per harness.
type Harnesses struct {
	Capabilities []v1alpha1.AdapterCapabilities `json:"capabilities"`
	KnownModels  map[string][]string            `json:"knownModels"`
}

func embeddedHarnesses() Harnesses {
	var h Harnesses
	if err := strictUnmarshal(defaultHarnesses, &h); err != nil {
		// The file is embedded and covered by tests; a failure here is a build defect.
		return Harnesses{}
	}
	return h
}

// DefaultCapabilities returns the embedded capability descriptors.
func DefaultCapabilities() []v1alpha1.AdapterCapabilities { return embeddedHarnesses().Capabilities }

// DefaultKnownModels returns the embedded known model ids, by harness.
func DefaultKnownModels() map[string][]string { return embeddedHarnesses().KnownModels }

// ParseCapabilities reads a capabilities file (`factory validate --capabilities`): a JSON
// list of v1alpha1.AdapterCapabilities, or an object with a "capabilities" list (the
// embedded defaults' format).
func ParseCapabilities(data []byte) ([]v1alpha1.AdapterCapabilities, error) {
	data = bytes.TrimSpace(data)
	var caps []v1alpha1.AdapterCapabilities
	if len(data) > 0 && data[0] == '{' {
		var h struct {
			Capabilities []v1alpha1.AdapterCapabilities `json:"capabilities"`
			KnownModels  json.RawMessage                `json:"knownModels"`
		}
		if err := strictUnmarshal(data, &h); err != nil {
			return nil, fmt.Errorf("capabilities: %w", err)
		}
		caps = h.Capabilities
	} else if err := strictUnmarshal(data, &caps); err != nil {
		return nil, fmt.Errorf("capabilities: %w", err)
	}
	if caps == nil {
		caps = []v1alpha1.AdapterCapabilities{}
	}
	for i, c := range caps {
		if c.Harness == "" {
			return nil, fmt.Errorf("capabilities[%d]: harness is required", i)
		}
		if slices.ContainsFunc(caps[:i], func(o v1alpha1.AdapterCapabilities) bool { return o.Harness == c.Harness }) {
			return nil, fmt.Errorf("capabilities[%d]: harness %q listed twice", i, c.Harness)
		}
	}
	return caps, nil
}

// ParseKnownModels reads a known-models file (`factory validate --known-models`): a JSON
// object of model ids by harness, e.g. {"codex": ["gpt-5-codex"]}.
func ParseKnownModels(data []byte) (map[string][]string, error) {
	var m map[string][]string
	if err := strictUnmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("known models: %w", err)
	}
	if m == nil {
		return nil, errors.New("known models: want an object of model ids by harness")
	}
	return m, nil
}

func strictUnmarshal(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("unexpected data after the value")
	}
	return nil
}

// capabilitiesFor returns the descriptor of a harness.
func (l *loader) capabilitiesFor(harness string) (v1alpha1.AdapterCapabilities, bool) {
	for _, c := range l.opts.Capabilities {
		if c.Harness == harness {
			return c, true
		}
	}
	return v1alpha1.AdapterCapabilities{}, false
}
