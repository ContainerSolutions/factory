package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/invopop/jsonschema"
)

// LimitsMemoryPattern is what limits.memory must match: a whole number of GiB with the Gi
// suffix, the form (and the regular expression) of the agent's workspace.size.memory.
const LimitsMemoryPattern = `^[1-9][0-9]{0,5}Gi$`

var limitsMemoryRE = regexp.MustCompile(LimitsMemoryPattern)

// Limits validation errors. Limits.Validate wraps one in a *ValidationError.
var (
	ErrLimitsCPUs   = errors.New("invalid limits.cpus")
	ErrLimitsMemory = errors.New("invalid limits.memory")
)

// Validate checks the workspace size a task asks for: cpus at least 1 and memory matching
// LimitsMemoryPattern, each when set. A nil Limits is valid. That the size is at most the
// agent's workspace.size is the agent's to enforce (it caps the request).
func (l *Limits) Validate() error {
	if l == nil {
		return nil
	}
	if l.CPUs < 0 {
		return &ValidationError{Field: "cpus", Message: fmt.Sprintf("%d: want at least 1", l.CPUs), Err: ErrLimitsCPUs}
	}
	if l.Memory != "" && !limitsMemoryRE.MatchString(l.Memory) {
		return &ValidationError{
			Field:   "memory",
			Message: fmt.Sprintf("%q: want a whole number of GiB like 4Gi (%s)", l.Memory, LimitsMemoryPattern),
			Err:     ErrLimitsMemory,
		}
	}
	return nil
}

// JSONSchemaExtend constrains cpus (at least 1) and memory (LimitsMemoryPattern) the way
// Validate does.
func (Limits) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("cpus"); ok {
		p.Minimum = "1"
	}
	if p, ok := s.Properties.Get("memory"); ok {
		p.Pattern = LimitsMemoryPattern
	}
}
