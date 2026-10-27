package catalog

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// kindSchemas are the compiled JSON Schemas of the config kinds, by kind. They are generated
// from the v1alpha1 types (v1alpha1.Schemas, the same bytes as schemas/*.schema.json), so
// they're compiled once per process.
var kindSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	c, err := newSchemaCompiler()
	if err != nil {
		return nil, err
	}
	out := map[string]*jsonschema.Schema{}
	for _, k := range v1alpha1.ConfigKinds() {
		s, err := c.Compile(v1alpha1.SchemaBaseURL + v1alpha1.SchemaFile(k))
		if err != nil {
			return nil, fmt.Errorf("compile the %s schema: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
})

// newSchemaCompiler returns a compiler that knows the v1alpha1 schemas by their $id and
// loads nothing else: no file system, no network.
func newSchemaCompiler() (*jsonschema.Compiler, error) {
	gen, err := v1alpha1.Schemas()
	if err != nil {
		return nil, fmt.Errorf("generate the v1alpha1 schemas: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	names := make([]string, 0, len(gen))
	for n := range gen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen[n]))
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", n, err)
		}
		if err := c.AddResource(v1alpha1.SchemaBaseURL+n, doc); err != nil {
			return nil, fmt.Errorf("schema %s: %w", n, err)
		}
	}
	return c, nil
}

// schemaProblem is one JSON Schema violation, at a path in the document.
type schemaProblem struct {
	path    []string
	message string
}

// schemaProblems flattens a validation error into its leaf violations. A failed oneOf/anyOf
// is reported once, with its alternatives' violations in the message, rather than as one
// error per alternative.
func schemaProblems(err error) []schemaProblem {
	ve, ok := err.(*jsonschema.ValidationError) //nolint:errorlint // Validate returns this type unwrapped
	if !ok {
		return []schemaProblem{{message: err.Error()}}
	}
	var out []schemaProblem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		switch e.ErrorKind.(type) {
		case *kind.OneOf, *kind.AnyOf:
			alts := leaves(e)
			msg := kindMessage(e.ErrorKind)
			if len(alts) > 0 {
				msg += ": " + strings.Join(alts, "; ")
			}
			out = append(out, schemaProblem{path: e.InstanceLocation, message: msg})
			return
		}
		if len(e.Causes) == 0 {
			out = append(out, schemaProblem{path: e.InstanceLocation, message: kindMessage(e.ErrorKind)})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// leaves returns the distinct leaf messages under e, prefixed with their location relative to
// e when it differs.
func leaves(e *jsonschema.ValidationError) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(c *jsonschema.ValidationError)
	walk = func(c *jsonschema.ValidationError) {
		if len(c.Causes) == 0 {
			msg := kindMessage(c.ErrorKind)
			if rel := c.InstanceLocation[min(len(e.InstanceLocation), len(c.InstanceLocation)):]; len(rel) > 0 {
				msg = strings.Join(rel, ".") + ": " + msg
			}
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
			return
		}
		for _, cc := range c.Causes {
			walk(cc)
		}
	}
	for _, c := range e.Causes {
		walk(c)
	}
	return out
}

// kindMessage renders one error kind with the library's default (English) printer.
func kindMessage(k jsonschema.ErrorKind) string {
	out := (&jsonschema.ValidationError{ErrorKind: k}).BasicOutput()
	if out.Error == nil {
		return "invalid"
	}
	return out.Error.String()
}
