package v1alpha1

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
)

// MaxWorkspaceHosts is the most entries a TaskSpec's workspace.hosts (and so a Task's
// spec.hosts) may list.
const MaxWorkspaceHosts = 20

// HostPattern is what a hosts entry must match: at least two lowercase RFC 1123 labels, an
// optional single leading "*." label followed by at least two more, a last label that starts
// with a letter (so no IPv4 literal fits) and an optional port 1-65535. No scheme, path or
// userinfo. The schema uses the same pattern; ValidateHost also checks the total length and
// refuses localhost.
const HostPattern = `^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?` +
	`(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`

// localhostPattern is what a hosts entry must not match: localhost or a name under
// it (RFC 6761), with or without a wildcard or port. The pipeline adds the local stack itself.
const localhostPattern = `(^|\.)localhost(:|$)`

var (
	hostRE  = regexp.MustCompile(HostPattern)
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Host validation errors. A *HostError wraps exactly one of them; Workspace.Validate and
// Task.Validate wrap it in a *ValidationError, so errors.Is works on a decode error too.
var (
	ErrHostSyntax    = errors.New("not a hostname")
	ErrHostWildcard  = errors.New("wildcard not allowed")
	ErrHostIP        = errors.New("IP literal not allowed")
	ErrHostLocalhost = errors.New("localhost not allowed")
	ErrHostPort      = errors.New("invalid port")
	ErrTooManyHosts  = errors.New("too many hosts")
)

// HostError is a hosts entry that ValidateHost refuses.
type HostError struct {
	Host   string
	Reason string
	// Err is one of the ErrHost* sentinels.
	Err error
}

func (e *HostError) Error() string {
	return fmt.Sprintf("%q: %v: %s", e.Host, e.Err, e.Reason)
}

func (e *HostError) Unwrap() error { return e.Err }

// ValidateHost checks one hosts entry: a lowercase hostname of at least two RFC 1123 labels,
// at most 253 characters, with an optional port and an optional single leading "*." label
// followed by at least two labels (*.example.com, not *.com). It refuses schemes, paths,
// userinfo, other wildcards, single-label names, IP literals and localhost, returning a
// *HostError.
func ValidateHost(h string) error {
	bad := func(err error, reason string) error { return &HostError{Host: h, Reason: reason, Err: err} }
	switch {
	case h == "":
		return bad(ErrHostSyntax, "empty")
	case strings.Contains(h, "://"):
		return bad(ErrHostSyntax, "write the hostname without a scheme")
	case strings.ContainsAny(h, "/?#"):
		return bad(ErrHostSyntax, "write the hostname without a path")
	case strings.Contains(h, "@"):
		return bad(ErrHostSyntax, "write the hostname without userinfo")
	}
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return bad(ErrHostIP, "list a hostname")
	}
	name := h
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		name = h[:i]
		if net.ParseIP(strings.Trim(name, "[]")) != nil {
			return bad(ErrHostIP, "list a hostname")
		}
		p := h[i+1:]
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || p[0] == '0' || strings.ContainsAny(p, "+-") {
			return bad(ErrHostPort, "the port must be 1-65535")
		}
	}
	if name != strings.ToLower(name) {
		return bad(ErrHostSyntax, "must be lowercase")
	}
	wildcard := strings.Contains(name, "*")
	if wildcard {
		rest, ok := strings.CutPrefix(name, "*.")
		if !ok || strings.Contains(rest, "*") {
			return bad(ErrHostWildcard, `only a single leading "*." label may be a wildcard`)
		}
		name = rest
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return bad(ErrHostLocalhost, "the local stack is added by the pipeline")
	}
	if len(name) > 253 {
		return bad(ErrHostSyntax, "longer than 253 characters")
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return bad(ErrHostSyntax, fmt.Sprintf("label %q is not an RFC 1123 label", l))
		}
	}
	if len(labels) < 2 {
		if wildcard {
			return bad(ErrHostWildcard, `"*." must be followed by at least two labels, e.g. *.example.com`)
		}
		return bad(ErrHostSyntax, "a hostname needs at least two labels, e.g. example.com")
	}
	if last := labels[len(labels)-1]; last[0] < 'a' || last[0] > 'z' {
		return bad(ErrHostSyntax, fmt.Sprintf("the last label %q must start with a letter", last))
	}
	if !hostRE.MatchString(h) {
		return bad(ErrHostSyntax, "does not match "+HostPattern)
	}
	return nil
}

// validateHosts checks a hosts list: at most MaxWorkspaceHosts entries, each one ValidateHost
// accepts. Field paths are relative to the list's parent (hosts, hosts[i]).
func validateHosts(hosts []string) error { return validateHostList("hosts", hosts) }

// validateHostList is validateHosts for a list in the field named field (hosts, allow).
func validateHostList(field string, hosts []string) error {
	if len(hosts) > MaxWorkspaceHosts {
		return &ValidationError{
			Field:   field,
			Message: fmt.Sprintf("%d hosts: at most %d", len(hosts), MaxWorkspaceHosts),
			Err:     ErrTooManyHosts,
		}
	}
	for i, h := range hosts {
		if err := ValidateHost(h); err != nil {
			return &ValidationError{Field: fmt.Sprintf("%s[%d]", field, i), Message: err.Error(), Err: err}
		}
	}
	return nil
}

// extendHostsSchema constrains the "hosts" property of s the way validateHosts does, except
// for the 253-character limit: at most MaxWorkspaceHosts entries matching HostPattern and
// not localhost.
func extendHostsSchema(s *jsonschema.Schema) { extendHostListSchema(s, "hosts") }

// extendHostListSchema is extendHostsSchema for the property named field (hosts, allow).
func extendHostListSchema(s *jsonschema.Schema, field string) {
	p, ok := s.Properties.Get(field)
	if !ok {
		return
	}
	p.MaxItems = ptr(uint64(MaxWorkspaceHosts))
	p.Items = &jsonschema.Schema{
		Type:    "string",
		Pattern: HostPattern,
		Not:     &jsonschema.Schema{Pattern: localhostPattern},
	}
}

// Validate checks hosts (validateHosts) and the environment (Environment.Validate).
func (w *Workspace) Validate() error {
	if err := validateHosts(w.Hosts); err != nil {
		return err
	}
	if w.Environment != nil {
		if err := w.Environment.Validate(); err != nil {
			return prefixField("environment", err)
		}
	}
	return nil
}

// JSONSchemaExtend constrains hosts (extendHostsSchema).
func (Workspace) JSONSchemaExtend(s *jsonschema.Schema) { extendHostsSchema(s) }

// Validate checks the task's hosts (validateHosts), environment (Environment.Validate) and
// versions (validateVersions).
func (t *Task) Validate() error {
	if err := validateHosts(t.Spec.Hosts); err != nil {
		return prefixField("spec", err)
	}
	if env := t.Spec.Environment; env != nil {
		if err := env.Validate(); err != nil {
			return prefixField("spec.environment", err)
		}
	}
	if err := validateVersions(t.Spec.Versions); err != nil {
		return prefixField("spec", err)
	}
	return nil
}

// JSONSchemaExtend constrains hosts (extendHostsSchema) and versions (extendVersionsSchema).
func (TaskRunSpec) JSONSchemaExtend(s *jsonschema.Schema) {
	extendHostsSchema(s)
	extendVersionsSchema(s)
}
