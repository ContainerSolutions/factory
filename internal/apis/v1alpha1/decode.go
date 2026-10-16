package v1alpha1

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Decoding errors, wrapped by DocumentError.
var (
	ErrUnknownKind = errors.New("unknown kind")
	ErrMissingKind = errors.New("missing kind")
	ErrAPIVersion  = errors.New("unsupported apiVersion")
)

// DocumentError is an error in one document of a (multi-document) YAML stream.
type DocumentError struct {
	// Index is the 0-based index of the document among the non-empty documents.
	Index int
	// Line is the 1-based line where the document starts.
	Line int
	// Kind is the document's kind, when it could be read.
	Kind string
	Err  error
}

func (e *DocumentError) Error() string {
	kind := e.Kind
	if kind == "" {
		kind = "?"
	}
	return fmt.Sprintf("document[%d] (line %d, kind %s): %v", e.Index, e.Line, kind, e.Err)
}

func (e *DocumentError) Unwrap() error { return e.Err }

// ValidationError is a rule the types enforce beyond the schema of each field, such as the
// Action one-of.
type ValidationError struct {
	// Field is the path of the offending field, e.g. spec.actions[1].
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func prefixField(prefix string, err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		field := prefix
		if ve.Field != "" {
			field += "." + ve.Field
		}
		return &ValidationError{Field: field, Message: ve.Message}
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

type validator interface{ Validate() error }

// Decode decodes a YAML stream of one or more documents separated by `---` lines. Each
// document is decoded strictly into the type of its kind: unknown fields, unknown kinds and
// an apiVersion other than APIVersion are errors. A missing apiVersion is accepted. Empty
// documents are skipped.
func Decode(data []byte) ([]Object, error) {
	var out []Object
	for i, doc := range splitDocuments(data) {
		obj, err := decodeOne(doc.body)
		if err != nil {
			de := &DocumentError{Index: i, Line: doc.line, Err: err}
			var ke *kindError
			if errors.As(err, &ke) {
				de.Kind, de.Err = ke.kind, ke.err
			}
			return nil, de
		}
		out = append(out, obj)
	}
	return out, nil
}

// DecodeOne decodes a stream that must hold exactly one document.
func DecodeOne(data []byte) (Object, error) {
	objs, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("want exactly one document, got %d", len(objs))
	}
	return objs[0], nil
}

// DecodeResult strictly decodes a Result (JSON or YAML).
func DecodeResult(data []byte) (*Result, error) {
	var r Result
	if err := yaml.UnmarshalStrict(data, &r); err != nil {
		return nil, fmt.Errorf("result: %w", err)
	}
	return &r, nil
}

// Encode marshals an object to YAML. It writes apiVersion only when the object has one.
func Encode(obj Object) ([]byte, error) {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode %s %q: %w", obj.GetTypeMeta().Kind, obj.GetName(), err)
	}
	return b, nil
}

type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

func decodeOne(doc []byte) (Object, error) {
	var tm TypeMeta
	if err := yaml.Unmarshal(doc, &tm); err != nil {
		return nil, err
	}
	if tm.Kind == "" {
		return nil, ErrMissingKind
	}
	if tm.APIVersion != "" && tm.APIVersion != APIVersion {
		return nil, &kindError{tm.Kind, fmt.Errorf("%w %q: want %q", ErrAPIVersion, tm.APIVersion, APIVersion)}
	}
	obj, err := New(tm.Kind)
	if err != nil {
		return nil, &kindError{tm.Kind, err}
	}
	if err := yaml.UnmarshalStrict(doc, obj); err != nil {
		return nil, &kindError{tm.Kind, err}
	}
	if v, ok := obj.(validator); ok {
		if err := v.Validate(); err != nil {
			return nil, &kindError{tm.Kind, err}
		}
	}
	return obj, nil
}

type document struct {
	line int
	body []byte
}

// splitDocuments splits a YAML stream on `---` lines (a separator may carry content after a
// space, e.g. `--- # comment`). Documents with no content are dropped.
func splitDocuments(data []byte) []document {
	var (
		docs  []document
		cur   bytes.Buffer
		start = 1
		line  = 0
	)
	flush := func() {
		body := append([]byte(nil), cur.Bytes()...)
		if hasContent(body) {
			docs = append(docs, document{line: start, body: body})
		}
		cur.Reset()
	}
	for _, text := range strings.Split(string(data), "\n") {
		line++
		text = strings.TrimSuffix(text, "\r")
		if text == "---" || strings.HasPrefix(text, "--- ") || strings.HasPrefix(text, "---\t") {
			flush()
			start = line
			text = strings.TrimSpace(text[3:])
			if text == "" || strings.HasPrefix(text, "#") {
				start = line + 1
				continue
			}
		}
		cur.WriteString(text)
		cur.WriteByte('\n')
	}
	flush()
	return docs
}

// hasContent reports whether a document has anything but blank lines and comments.
func hasContent(doc []byte) bool {
	for _, l := range strings.Split(string(doc), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			return true
		}
	}
	return false
}
