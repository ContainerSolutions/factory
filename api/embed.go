// Package api holds the factory's HTTP contract: openapi.yaml and one example per operation's
// success response under examples/. Both are embedded, so factory-mock and the contract tests
// read the same files the code generators and the web app do: the files in this directory are
// the only copy.
//
// An example is examples/<operationId>.json. Its content depends on the response's media type:
//   - a JSON media type (application/json, …+json): the body itself;
//   - text/event-stream: a JSON array of frames, {"event", "id", "data"};
//   - application/gzip: a JSON string holding the uncompressed body, gzip-compressed when
//     served (the transcript, JSON Lines);
//   - any other media type (text/plain): a JSON string holding the body.
//
// Operations whose success response has no body (204, 202 without content, 302) have no
// example file.
package api

import (
	"context"
	"embed"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
)

// Spec is api/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte

// Examples holds examples/<operationId>.json.
//
//go:embed examples/*.json
var Examples embed.FS

// ExamplesDir is the directory of the examples inside Examples.
const ExamplesDir = "examples"

// Load parses and validates the embedded spec.
func Load(ctx context.Context) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	doc, err := loader.LoadFromData(Spec)
	if err != nil {
		return nil, fmt.Errorf("load openapi.yaml: %w", err)
	}
	if err := doc.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate openapi.yaml: %w", err)
	}
	return doc, nil
}
