package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Media types with their own example rules.
const (
	// MediaTypeSSE is the media type of server-sent events.
	MediaTypeSSE = "text/event-stream"
	// MediaTypeGzip is the media type of gzip-compressed bodies (transcripts).
	MediaTypeGzip = "application/gzip"
)

// Operation is one operation of the spec, flattened for routing and examples.
type Operation struct {
	ID     string
	Method string
	// Path is the full path template: the effective server's path followed by the spec path,
	// e.g. /api/v1/tasks/{id} or /hooks/{provider}.
	Path string
	Tags []string
	// Status is the success status: the lowest 2xx response with a body, else the lowest 2xx
	// response, else the lowest 3xx (the OAuth redirects).
	Status int
	// ContentType is the success response's media type; empty when it has no body.
	ContentType string
	// Media is the success response's media type object; nil when it has no body.
	Media *openapi3.MediaType
	// Operation is the spec's operation.
	Operation *openapi3.Operation
}

// HasBody reports whether the success response has a body, and so an example.
func (o Operation) HasBody() bool { return o.ContentType != "" }

// Operations lists the spec's operations, sorted by path then method.
func Operations(doc *openapi3.T) ([]Operation, error) {
	if doc.Paths == nil {
		return nil, errors.New("spec has no paths")
	}
	paths := doc.Paths.Map()
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Operation
	for _, p := range keys {
		item := paths[p]
		ops := item.Operations()
		methods := make([]string, 0, len(ops))
		for m := range ops {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		for _, m := range methods {
			op := ops[m]
			base, err := serverPath(doc.Servers, item.Servers, op.Servers)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", m, p, err)
			}
			o := Operation{ID: op.OperationID, Method: m, Path: base + p, Tags: op.Tags, Operation: op}
			if err := o.setSuccess(); err != nil {
				return nil, fmt.Errorf("%s %s: %w", m, p, err)
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// serverPath is the path of the effective server's URL: the operation's servers, else the
// path item's, else the document's. "/" and a host-only URL give "".
func serverPath(doc, item openapi3.Servers, op *openapi3.Servers) (string, error) {
	servers := doc
	if len(item) > 0 {
		servers = item
	}
	if op != nil && len(*op) > 0 {
		servers = *op
	}
	if len(servers) == 0 {
		return "", nil
	}
	u, err := url.Parse(servers[0].URL)
	if err != nil {
		return "", fmt.Errorf("server URL %q: %w", servers[0].URL, err)
	}
	return strings.TrimSuffix(u.Path, "/"), nil
}

func (o *Operation) setSuccess() error {
	if o.Operation.Responses == nil {
		return errors.New("no responses")
	}
	var codes, redirects []int
	for k := range o.Operation.Responses.Map() {
		code, err := strconv.Atoi(k)
		switch {
		case err != nil:
		case code >= 200 && code <= 299:
			codes = append(codes, code)
		case code >= 300 && code <= 399:
			redirects = append(redirects, code)
		}
	}
	if len(codes) == 0 {
		codes = redirects
	}
	if len(codes) == 0 {
		return errors.New("no 2xx or 3xx response")
	}
	sort.Ints(codes)
	o.Status = codes[0]
	for _, code := range codes {
		ref := o.Operation.Responses.Status(code)
		if ref == nil || ref.Value == nil || len(ref.Value.Content) == 0 {
			continue
		}
		if len(ref.Value.Content) != 1 {
			return fmt.Errorf("response %d has %d media types, want 1", code, len(ref.Value.Content))
		}
		for ct, media := range ref.Value.Content {
			o.Status, o.ContentType, o.Media = code, ct, media
		}
		return nil
	}
	return nil
}

// IsJSON reports whether a media type is JSON (application/json or a +json suffix).
func IsJSON(mediaType string) bool {
	mt, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// ExampleFile is the name of an operation's example inside Examples.
func ExampleFile(operationID string) string {
	return path.Join(ExamplesDir, operationID+".json")
}

// Example reads an operation's example file.
func (o Operation) Example() ([]byte, error) {
	b, err := fs.ReadFile(Examples, ExampleFile(o.ID))
	if err != nil {
		return nil, fmt.Errorf("example for %s: %w", o.ID, err)
	}
	return b, nil
}

// ExampleBody renders a non-SSE example as the response body: the JSON itself for a JSON
// media type, the decoded string gzip-compressed for application/gzip, the decoded string
// otherwise. The output is deterministic.
func (o Operation) ExampleBody() ([]byte, error) {
	if !o.HasBody() || o.ContentType == MediaTypeSSE {
		return nil, fmt.Errorf("%s: no plain body example (content type %q)", o.ID, o.ContentType)
	}
	b, err := o.Example()
	if err != nil {
		return nil, err
	}
	if IsJSON(o.ContentType) {
		if !json.Valid(b) {
			return nil, fmt.Errorf("example for %s is not valid JSON", o.ID)
		}
		return b, nil
	}
	s, err := o.ExampleText()
	if err != nil {
		return nil, err
	}
	if o.ContentType != MediaTypeGzip {
		return []byte(s), nil
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z) // no name or mtime in the header: deterministic
	if _, err := w.Write([]byte(s)); err != nil {
		return nil, fmt.Errorf("gzip example for %s: %w", o.ID, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("gzip example for %s: %w", o.ID, err)
	}
	return z.Bytes(), nil
}

// ExampleText decodes the example of a non-JSON, non-SSE media type: the JSON string the file
// holds (for application/gzip, the uncompressed body).
func (o Operation) ExampleText() (string, error) {
	if !o.HasBody() || o.ContentType == MediaTypeSSE || IsJSON(o.ContentType) {
		return "", fmt.Errorf("%s: no text example (content type %q)", o.ID, o.ContentType)
	}
	b, err := o.Example()
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("example for %s (%s) must be a JSON string: %w", o.ID, o.ContentType, err)
	}
	return s, nil
}

// Frame is one server-sent event of an SSE example.
type Frame struct {
	Event string          `json:"event"`
	ID    string          `json:"id,omitzero"`
	Data  json.RawMessage `json:"data"`
}

// ExampleFrames decodes an SSE example.
func (o Operation) ExampleFrames() ([]Frame, error) {
	if o.ContentType != MediaTypeSSE {
		return nil, fmt.Errorf("%s: not an event stream (content type %q)", o.ID, o.ContentType)
	}
	b, err := o.Example()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var frames []Frame
	if err := dec.Decode(&frames); err != nil {
		return nil, fmt.Errorf("example for %s must be an array of {event, id, data}: %w", o.ID, err)
	}
	for i, f := range frames {
		if f.Event == "" || len(f.Data) == 0 {
			return nil, fmt.Errorf("example for %s: frame %d needs event and data", o.ID, i)
		}
	}
	return frames, nil
}

// Encode writes the frame in the text/event-stream format, with its data on one line.
func (f Frame) Encode() ([]byte, error) {
	var data bytes.Buffer
	if err := json.Compact(&data, f.Data); err != nil {
		return nil, fmt.Errorf("frame %q: %w", f.Event, err)
	}
	var b bytes.Buffer
	b.WriteString("event: " + f.Event + "\n")
	if f.ID != "" {
		b.WriteString("id: " + f.ID + "\n")
	}
	b.WriteString("data: ")
	b.Write(data.Bytes())
	b.WriteString("\n\n")
	return b.Bytes(), nil
}
