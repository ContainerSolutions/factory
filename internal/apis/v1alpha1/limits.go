package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/invopop/jsonschema"
)

// LimitsMemoryPattern is a whole number of GiB with the Gi suffix: the form of limits.memory
// and of the agent's workspace.size.memory and .disk (config.GiB parses all three with it).
// limits.memory must also be at least WorkspaceMinMemoryGiB (limitsMemoryMinPattern).
const LimitsMemoryPattern = `^[1-9][0-9]{0,5}Gi$`

// WorkspaceMinMemoryGiB is the least memory a workspace VM has, on every driver: limits.memory's
// minimum, and the agent config's for workspace.size.memory.
const WorkspaceMinMemoryGiB = 2

// limitsMemoryMinPattern is what limits.memory must match: LimitsMemoryPattern from
// WorkspaceMinMemoryGiB up. Validate and the schemas use it; TestLimitsMemoryMinPattern keeps it
// in step with the two constants.
const limitsMemoryMinPattern = `^([2-9]|[1-9][0-9]{1,5})Gi$`

var limitsMemoryRE = regexp.MustCompile(limitsMemoryMinPattern)

// Limits validation errors. Limits.Validate wraps one in a *ValidationError.
var (
	ErrLimitsCPUs   = errors.New("invalid limits.cpus")
	ErrLimitsMemory = errors.New("invalid limits.memory")
)

// Validate checks the workspace size a task asks for: cpus at least 1 and memory a whole
// number of GiB, at least WorkspaceMinMemoryGiB (limitsMemoryMinPattern), each when set. A nil
// Limits is valid. That the size is at most the agent's workspace.size is the agent's to
// enforce (it caps the request).
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
			Message: fmt.Sprintf("%q: want a whole number of GiB, at least %dGi, like 4Gi (%s)", l.Memory, WorkspaceMinMemoryGiB, limitsMemoryMinPattern),
			Err:     ErrLimitsMemory,
		}
	}
	return nil
}

// JSONSchemaExtend constrains cpus (at least 1) and memory (limitsMemoryMinPattern) the way
// Validate does.
func (Limits) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("cpus"); ok {
		p.Minimum = "1"
	}
	if p, ok := s.Properties.Get("memory"); ok {
		p.Pattern = limitsMemoryMinPattern
	}
}
