package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// Line is the 1-based line of the document's root node; 0 when unknown (YAML syntax errors
	// carry their line in Err).
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
	if e.Line == 0 {
		return fmt.Sprintf("document[%d] (kind %s): %v", e.Index, kind, e.Err)
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

// Decode decodes a YAML 1.2 stream of one or more documents (separated by `---`, optionally
// ended by `...`). Each document is decoded strictly into the type of its kind: unknown
// fields, duplicate keys, non-string keys, unknown kinds and an apiVersion other than
// APIVersion are errors. A missing apiVersion is accepted. Empty documents are skipped.
func Decode(data []byte) ([]Object, error) {
	docs, err := yamlDocuments(data)
	if err != nil {
		de := &DocumentError{Index: len(docs), Err: err}
		var le *lineError
		if errors.As(err, &le) {
			de.Line, de.Err = le.line, le.err
		}
		return nil, de
	}
	out := make([]Object, 0, len(docs))
	for i, doc := range docs {
		obj, err := decodeOne(doc.json)
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
	if err := unmarshalYAML(data, &r); err != nil {
		return nil, fmt.Errorf("result: %w", err)
	}
	return &r, nil
}

// Encode marshals an object to block-style YAML. It writes apiVersion only when the object
// has one.
func Encode(obj Object) ([]byte, error) {
	b, err := marshalYAML(obj)
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

// decodeOne decodes one document, already converted to JSON.
func decodeOne(doc []byte) (Object, error) {
	var tm TypeMeta
	if err := json.Unmarshal(doc, &tm); err != nil {
		return nil, fmt.Errorf("want a mapping with kind: %w", err)
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
	if err := strictJSON(doc, obj); err != nil {
		return nil, &kindError{tm.Kind, err}
	}
	if v, ok := obj.(validator); ok {
		if err := v.Validate(); err != nil {
			return nil, &kindError{tm.Kind, err}
		}
	}
	return obj, nil
}
