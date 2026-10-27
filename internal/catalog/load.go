package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// Directories that hold assets, or nothing the loader reads as objects.
const (
	promptsDir  = "prompts"
	schemasDir  = "schemas"
	testdataDir = "testdata"
)

// assetBaseURL is the base under which catalog JSON Schemas are registered with the
// compiler, so relative $refs between them resolve. The .invalid TLD never resolves, and the
// compiler has no loader anyway.
const assetBaseURL = "https://catalog.invalid/"

// maxNodes bounds the nodes one document may expand to through aliases.
const maxNodes = 1 << 20

// document is one decoded object and where it came from.
type document struct {
	file string
	root *yaml.Node
	obj  v1alpha1.Object
	key  ObjectKey
}

// line returns the line of the field at p in the document.
func (d *document) line(p fieldPath) int { return lineOf(d.root, p) }

// loader holds the state of one LoadDir call.
type loader struct {
	fsys fs.FS
	opts Options
	errs []Error

	files   map[string]bool // every regular file, relative to the root
	sum     hash.Hash
	docs    []*document
	prompts map[string]*Prompt
	schemas map[string]*jsonschema.Schema
	// schemaSources are the raw *.json files under schemas/, compiled after the walk;
	// schemaRaw are those that compiled (the raw bytes of schemas).
	schemaSources map[string][]byte
	schemaRaw     map[string][]byte
}

// LoadDir loads the catalog rooted at fsys and validates it (every phase of the LLD, all
// diagnostics collected). It returns the snapshot when there are no errors (warnings
// allowed, unless opts.Strict), nil otherwise; the diagnostics are sorted by file and line.
func LoadDir(fsys fs.FS, opts Options) (*Catalog, []Error) {
	if opts.Capabilities == nil {
		opts.Capabilities = DefaultCapabilities()
	}
	if opts.KnownModels == nil {
		opts.KnownModels = DefaultKnownModels()
	}
	l := &loader{
		fsys:          fsys,
		opts:          opts,
		files:         map[string]bool{},
		sum:           sha256.New(),
		prompts:       map[string]*Prompt{},
		schemas:       map[string]*jsonschema.Schema{},
		schemaSources: map[string][]byte{},
		schemaRaw:     map[string][]byte{},
	}
	l.walk()
	l.compileSchemaAssets()
	c := l.validate()
	if l.opts.SHA != "" {
		c.SHA = l.opts.SHA
	} else {
		c.SHA = "dir:" + hex.EncodeToString(l.sum.Sum(nil))
	}
	errs := l.errs
	if opts.Strict {
		for i := range errs {
			errs[i].Severity = SeverityError
		}
	}
	errs = sortErrors(errs)
	if HasErrors(errs) {
		return nil, errs
	}
	return c, errs
}

// add records a diagnostic.
func (l *loader) add(sev Severity, code, file string, line int, obj ObjectKey, format string, args ...any) {
	l.errs = append(l.errs, Error{
		Code: code, Message: fmt.Sprintf(format, args...), File: file, Line: line, Object: obj, Severity: sev,
	})
}

// errorf records an error at a field of a document; the message starts with the path.
func (l *loader) errorf(d *document, p fieldPath, code, format string, args ...any) {
	l.at(SeverityError, d, p, code, format, args...)
}

// warnf records a warning at a field of a document.
func (l *loader) warnf(d *document, p fieldPath, code, format string, args ...any) {
	l.at(SeverityWarning, d, p, code, format, args...)
}

func (l *loader) at(sev Severity, d *document, p fieldPath, code, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if len(p) > 0 {
		msg = p.String() + ": " + msg
	}
	l.add(sev, code, d.file, d.line(p), d.key, "%s", msg)
}

// walk reads every file under the root (lexical order), hashing them all, and sorts them
// into object documents and assets.
func (l *loader) walk() {
	err := fs.WalkDir(l.fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			l.add(SeverityError, CodeSyntax, p, 0, ObjectKey{}, "read: %v", err)
			if de != nil && de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if de.IsDir() {
			if p != "." && strings.HasPrefix(de.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() || strings.HasPrefix(de.Name(), ".") {
			return nil
		}
		data, err := fs.ReadFile(l.fsys, p)
		if err != nil {
			l.add(SeverityError, CodeSyntax, p, 0, ObjectKey{}, "read: %v", err)
			return nil
		}
		l.files[p] = true
		fmt.Fprintf(l.sum, "%s\x00%d\x00", p, len(data))
		l.sum.Write(data)
		l.classify(p, data)
		return nil
	})
	if err != nil {
		l.add(SeverityError, CodeSyntax, ".", 0, ObjectKey{}, "walk: %v", err)
	}
}

// classify routes one file: prompts/** and schemas/**/*.json are assets; *.yaml and *.yml
// anywhere else (except under prompts/, schemas/ or testdata/ at any depth) are objects.
func (l *loader) classify(p string, data []byte) {
	dirs := strings.Split(path.Dir(p), "/")
	switch dirs[0] {
	case promptsDir:
		l.loadPrompt(p, data)
		return
	case schemasDir:
		if path.Ext(p) == ".json" {
			l.schemaSources[p] = data
		}
		return
	}
	for _, d := range dirs {
		if d == promptsDir || d == schemasDir || d == testdataDir {
			return
		}
	}
	if ext := path.Ext(p); ext == ".yaml" || ext == ".yml" {
		l.loadObjects(p, data)
	}
}

// yamlLinePrefixRE is the "line N: " yaml.v3 puts in its messages; the line is reported
// separately.
var yamlLinePrefixRE = regexp.MustCompile(`^line \d+: `)

// oneLine joins a multi-line message (the JSON Schema library indents its causes) into one
// line.
func oneLine(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimLeft(strings.TrimSpace(l), "- "); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "; ")
}

// templateLineRE finds the line in a text/template parse error ("template: name:7: …").
var templateLineRE = regexp.MustCompile(`:(\d+):`)

func templateErrorLine(err error) int {
	if m := templateLineRE.FindStringSubmatch(err.Error()); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			return n
		}
	}
	return 0
}

func (l *loader) loadPrompt(p string, data []byte) {
	text := string(data)
	tmpl, err := template.New(p).Parse(text)
	if err != nil {
		l.add(SeverityError, CodeInvalidAsset, p, templateErrorLine(err), ObjectKey{}, "prompt template: %v", err)
		return
	}
	sum := sha256.Sum256(data)
	l.prompts[p] = &Prompt{Path: p, Text: text, Digest: "sha256:" + hex.EncodeToString(sum[:]), Template: tmpl}
}

// compileSchemaAssets compiles the JSON Schemas under schemas/. They are all registered
// first, so they can $ref each other (relative refs) and the v1alpha1 schemas (by $id).
func (l *loader) compileSchemaAssets() {
	if len(l.schemaSources) == 0 {
		return
	}
	c, err := newSchemaCompiler()
	if err != nil {
		l.add(SeverityError, CodeInvalidAsset, schemasDir, 0, ObjectKey{}, "%v", err)
		return
	}
	paths := make([]string, 0, len(l.schemaSources))
	for p := range l.schemaSources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var ok []string
	for _, p := range paths {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(l.schemaSources[p]))
		if err != nil {
			l.add(SeverityError, CodeInvalidAsset, p, 0, ObjectKey{}, "JSON Schema: %v", err)
			continue
		}
		if err := c.AddResource(assetBaseURL+p, doc); err != nil {
			l.add(SeverityError, CodeInvalidAsset, p, 0, ObjectKey{}, "JSON Schema: %v", err)
			continue
		}
		ok = append(ok, p)
	}
	for _, p := range ok {
		s, err := c.Compile(assetBaseURL + p)
		if err != nil {
			l.add(SeverityError, CodeInvalidAsset, p, 0, ObjectKey{}, "JSON Schema: %s", oneLine(strings.ReplaceAll(err.Error(), assetBaseURL, "")))
			continue
		}
		l.schemas[p] = s
		l.schemaRaw[p] = l.schemaSources[p]
	}
}

// loadObjects decodes every document of a YAML file: schema validation (phase 1) against the
// raw document, then strict decoding with v1alpha1.Decode.
func (l *loader) loadObjects(file string, data []byte) {
	schemas, schemaErr := kindSchemas()
	if schemaErr != nil {
		l.add(SeverityError, CodeSchema, file, 0, ObjectKey{}, "%v", schemaErr)
		return
	}
	for _, seg := range splitDocuments(data) {
		root, err := parseSegment(seg)
		if err != nil {
			l.add(SeverityError, CodeSyntax, file, errorLine(err, seg.start), ObjectKey{}, "%s", yamlLinePrefixRE.ReplaceAllString(strings.TrimPrefix(err.Error(), "yaml: "), ""))
			continue
		}
		if root == nil {
			continue
		}
		l.loadDocument(file, seg, root, schemas)
	}
}

func (l *loader) loadDocument(file string, seg segment, root *yaml.Node, schemas map[string]*jsonschema.Schema) {
	d := &document{file: file, root: root}
	budget := maxNodes
	raw, rawErr := nodeValue(root, &budget)
	if rawErr == nil {
		m, isMap := raw.(map[string]any)
		if !isMap {
			l.add(SeverityError, CodeNotAnObject, file, root.Line, ObjectKey{}, "document is not a mapping with apiVersion and kind")
			return
		}
		k, _ := m["kind"].(string)
		name := ""
		if md, ok := m["metadata"].(map[string]any); ok {
			name, _ = md["name"].(string)
		}
		d.key = ObjectKey{Kind: k, Name: name}
		switch {
		case k == "":
			l.add(SeverityError, CodeNotAnObject, file, root.Line, d.key, "document has no kind: every YAML document outside prompts/, schemas/ and testdata/ must be a catalog object")
			return
		case !v1alpha1.IsConfigKind(k):
			if _, err := v1alpha1.New(k); err == nil {
				l.add(SeverityError, CodeNotAnObject, file, root.Line, d.key, "kind %s is a runtime kind; the catalog holds %s", k, strings.Join(v1alpha1.ConfigKinds(), ", "))
			} else {
				l.add(SeverityError, CodeNotAnObject, file, root.Line, d.key, "unknown kind %q; the catalog holds %s", k, strings.Join(v1alpha1.ConfigKinds(), ", "))
			}
			return
		}
		if err := schemas[k].Validate(raw); err != nil {
			for _, p := range schemaProblems(err) {
				fp := pointerPath(p.path, root)
				l.errorf(d, fp, CodeSchema, "%s", p.message)
			}
		}
	}
	objs, err := v1alpha1.Decode(seg.data)
	if err != nil {
		var de *v1alpha1.DocumentError
		line, msg := root.Line, err.Error()
		if errors.As(err, &de) {
			if de.Line > 0 {
				line = de.Line
			}
			msg = de.Err.Error()
			if errors.Is(de.Err, v1alpha1.ErrMissingKind) {
				l.add(SeverityError, CodeNotAnObject, file, line, d.key, "document has no kind")
				return
			}
		}
		if rawErr == nil && l.reportedAt(file, root) {
			return // the schema errors above already explain it
		}
		l.add(SeverityError, CodeSchema, file, line, d.key, "%s", msg)
		return
	}
	for i, obj := range objs {
		dd := d
		if i > 0 { // a document after "..." without "---": no node of its own
			dd = &document{file: file, root: root}
		}
		dd.obj = obj
		dd.key = ObjectKey{Kind: obj.GetTypeMeta().Kind, Name: obj.GetName()}
		l.docs = append(l.docs, dd)
	}
}

// reportedAt reports whether a schema error was already recorded for the document whose root
// is root.
func (l *loader) reportedAt(file string, root *yaml.Node) bool {
	last := lastLine(root)
	for _, e := range l.errs {
		if e.File == file && e.Code == CodeSchema && e.Line >= root.Line && e.Line <= last {
			return true
		}
	}
	return false
}

// lastLine is the last line any node under n starts on.
func lastLine(n *yaml.Node) int {
	line := n.Line
	for _, c := range n.Content {
		line = max(line, lastLine(c))
	}
	return line
}
