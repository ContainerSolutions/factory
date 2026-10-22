// Package runbundle is the run bundle the supervisor writes for a VM workspace, the outcome
// the VM writes back and the event stream between them (workspace-run.md §The run bundle).
// It imports only v1alpha1, the apiclient and runner types and the standard library, and it
// has no network code. The bundle holds no secret: no field of any type here carries a token.
package runbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Version is the bundle format's version: bundle.json's "version".
const Version = 1

// DefaultDispatcherURL is the dispatcher as the VM sees it: the gateway serves the name and
// adds the run's credential (phase 4).
const DefaultDispatcherURL = "http://dispatcher.factory.internal"

// The bundle's files, relative to its root.
const (
	ManifestFile = "bundle.json"
	TaskFile     = "task.json"
	PromptFile   = "prompt.md"
	ContextDir   = "context"
	SchemaFile   = "output-schema.json"
	SkillsDir    = "skills"
	CAFile       = "gateway/ca.pem"
	CodexFile    = "codex/account.json"
)

// The caps Read and Write apply: the bounds the pipeline applies on the host (the prompt
// maxPrompt, a context item's content maxContext, the schema maxSchema). A context file is
// the item's JSON, so its file may be up to MaxContextFile.
const (
	MaxManifestBytes = 1 << 20
	MaxTaskBytes     = 1 << 20
	MaxPromptBytes   = 1 << 20
	MaxContextBytes  = 4 << 20
	MaxContextFile   = 2 * MaxContextBytes
	MaxSchemaBytes   = 1 << 20
	MaxCABytes       = 1 << 20
)

// Manifest is bundle.json.
type Manifest struct {
	Version int    `json:"version"`
	RunID   string `json:"runId"` // run_…; never a token
	LeaseID string `json:"leaseId"`
	TaskID  string `json:"taskId"`
	// StartedAt is the lease's start, for the record; limits use --remaining.
	StartedAt time.Time `json:"startedAt"`
	// Dispatcher is FACTORY_API in the VM; Write defaults it to DefaultDispatcherURL.
	Dispatcher string    `json:"dispatcher"`
	Gateway    Gateway   `json:"gateway"`
	Git        GitAccess `json:"git"`
	// Prompt, Context's files, OutputSchema ("" when the schema answered 404) and
	// Gateway.CAFile are paths relative to the bundle; Write sets them.
	Prompt       string        `json:"prompt"`
	Context      []ContextFile `json:"context,omitempty"`
	OutputSchema string        `json:"outputSchema,omitempty"`
	// Skills are the materialised skills, their Dir relative to the bundle.
	Skills []Skill `json:"skills,omitempty"`
	// Codex is set only when the task's harness is codex.
	Codex *CodexAccount `json:"codex,omitempty"`
}

// Gateway is what the VM needs to use its gateway; its address is the --proxy flag.
type Gateway struct {
	CAFile  string   `json:"caFile,omitempty"`
	NoProxy []string `json:"noProxy,omitempty"`
}

// GitAccess is the lease's gitCredential and readCredentials without their tokens. It has no
// token field: the gateway holds the tokens.
type GitAccess struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Repo     string `json:"repo"`
	// Permissions are the credential's permissions (contents: read|write, …).
	Permissions map[string]string `json:"permissions,omitempty"`
	// Reads are the read credentials' scopes.
	Reads []GitRead `json:"reads,omitempty"`
}

// GitRead is one read credential's scope.
type GitRead struct {
	Repo      string   `json:"repo"`
	ReadRepos []string `json:"readRepos,omitempty"`
}

// ContextFile is one non-repo context item: its file, or Gone when it answered 404.
type ContextFile struct {
	Index int    `json:"index"`
	File  string `json:"file,omitempty"`
	Gone  bool   `json:"gone,omitempty"`
}

// Skill is harness.Skill with Dir relative to the bundle: skills/<plugin>/skills/<name>.
type Skill struct {
	Name   string `json:"name"`
	Plugin string `json:"plugin"`
	Dir    string `json:"dir"`
}

// CodexAccount identifies the ChatGPT account codex runs as. Identifiers, not credentials
// (P1 §5): the gateway adds the credential.
type CodexAccount struct {
	AccountID string `json:"accountId"`
	UserID    string `json:"userId"`
	PlanType  string `json:"planType"`
}

// Files is what Write lays out beside bundle.json.
type Files struct {
	// Task is the lease's Task (never the Lease: it has the credentials).
	Task v1alpha1.Task
	// Prompt is GET /tasks/{id}/prompt, byte-exact.
	Prompt []byte
	// Context holds, by index, the apiclient.TaskContext JSON of each non-repo context item
	// that answered 200. A non-repo item missing from it is Gone.
	Context map[int][]byte
	// OutputSchema is GET /tasks/{id}/output-schema; nil when it answered 404.
	OutputSchema []byte
	// SkillsDir holds the materialised plugin dirs; "" when there are no skills.
	SkillsDir string
	// CA is the gateway CA's PEM (the public half).
	CA []byte
	// Codex is the account, only when the task's harness is codex.
	Codex *CodexAccount
}

// Bundle is a bundle Read and validated.
type Bundle struct {
	Dir      string
	Manifest Manifest
	Task     v1alpha1.Task
	Prompt   []byte
	// Context holds the items that have a file; Gone items are absent (see ContextItem).
	Context map[int]apiclient.TaskContext
	// OutputSchema is nil when the schema answered 404.
	OutputSchema []byte
	// CAFile is the gateway CA's absolute path; "" when the bundle has none.
	CAFile string
}

// ValidationError is a bundle, an outcome or an event that breaks a rule.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("run bundle: %s: %s", e.Field, e.Message)
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// tokenPatterns are the credential shapes no bundle field may hold: GitHub tokens, API keys
// (sk-…, sk-ant-… included), the run token (rtk_), an agent token (agt_<id>.<secret>) and
// private key blocks. They mirror the pipeline's redactor, plus the factory's own tokens.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\brtk_[A-Za-z0-9_.-]+`),
	regexp.MustCompile(`\bagt_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
}

// LooksLikeToken reports whether s holds something shaped like a credential.
func LooksLikeToken(s string) bool {
	for _, re := range tokenPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// scanTokens refuses any string in v (struct fields, slices, map keys and values) shaped
// like a token. The error names the field, never the value.
func scanTokens(v reflect.Value, field string) error {
	switch v.Kind() {
	case reflect.String:
		if LooksLikeToken(v.String()) {
			return invalid(field, "holds a value shaped like a token")
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return scanTokens(v.Elem(), field)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if name == "" {
				name = sf.Name
			}
			if field != "" {
				name = field + "." + name
			}
			if err := scanTokens(v.Field(i), name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := scanTokens(v.Index(i), fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := scanTokens(it.Key(), field+" (a key)"); err != nil {
				return err
			}
			if err := scanTokens(it.Value(), field); err != nil {
				return err
			}
		}
	}
	return nil
}

// nameRE is a plugin or skill name (the pipeline's materialise uses the same).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// Validate checks the rules that need only the manifest: the version, the ids, the
// dispatcher URL, every path, the skills, NoProxy and that no field is shaped like a token.
// Read and Write also check it against task.json.
func (m Manifest) Validate() error {
	if m.Version != Version {
		return invalid("version", "%d is not a known version (want %d)", m.Version, Version)
	}
	if err := scanTokens(reflect.ValueOf(m), ""); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(m.RunID, "rtk_"):
		return invalid("runId", "is a run token, not a run id")
	case !strings.HasPrefix(m.RunID, "run_") || len(m.RunID) == len("run_"):
		return invalid("runId", "%q is not a run id (run_…)", m.RunID)
	case m.TaskID == "":
		return invalid("taskId", "is empty")
	case m.LeaseID == "":
		return invalid("leaseId", "is empty")
	}
	if err := checkURL(m.Dispatcher); err != nil {
		return invalid("dispatcher", "%v", err)
	}
	if err := checkPath("prompt", m.Prompt, true); err != nil {
		return err
	}
	if err := checkPath("outputSchema", m.OutputSchema, false); err != nil {
		return err
	}
	if err := checkPath("gateway.caFile", m.Gateway.CAFile, false); err != nil {
		return err
	}
	for i, h := range m.Gateway.NoProxy {
		if _, _, err := net.ParseCIDR(h); err == nil {
			continue
		}
		if err := v1alpha1.ValidateHost(h); err != nil {
			return invalid(fmt.Sprintf("gateway.noProxy[%d]", i), "not a host or a CIDR: %v", err)
		}
	}
	for i, c := range m.Context {
		f := fmt.Sprintf("context[%d]", i)
		if c.Gone != (c.File == "") {
			return invalid(f, "has a file exactly when it isn't gone")
		}
		if err := checkPath(f+".file", c.File, false); err != nil {
			return err
		}
	}
	for i, s := range m.Skills {
		f := fmt.Sprintf("skills[%d]", i)
		if !nameRE.MatchString(s.Name) || !nameRE.MatchString(s.Plugin) {
			return invalid(f, "%q:%q is not a plugin and a skill name", s.Plugin, s.Name)
		}
		if want := path.Join(SkillsDir, s.Plugin, "skills", s.Name); s.Dir != want {
			return invalid(f+".dir", "%q is not %q", s.Dir, want)
		}
	}
	if c := m.Codex; c != nil && (c.AccountID == "" || c.UserID == "" || c.PlanType == "") {
		return invalid("codex", "needs an account id, a user id and a plan type")
	}
	return nil
}

// validateTask checks the manifest against task.json: the task id, the context items and
// the codex account.
func (m Manifest) validateTask(t v1alpha1.Task) error {
	if t.Metadata.ID != m.TaskID {
		return invalid("taskId", "%q is not task.json's %q", m.TaskID, t.Metadata.ID)
	}
	seen := map[int]bool{}
	for i, c := range m.Context {
		f := fmt.Sprintf("context[%d]", i)
		switch {
		case c.Index < 0 || c.Index >= len(t.Spec.Context):
			return invalid(f, "index %d is not one of the task's %d context items", c.Index, len(t.Spec.Context))
		case t.Spec.Context[c.Index].Kind == "repo":
			return invalid(f, "index %d is a repo item", c.Index)
		case seen[c.Index]:
			return invalid(f, "index %d is listed twice", c.Index)
		}
		seen[c.Index] = true
	}
	for i, c := range t.Spec.Context {
		if c.Kind != "repo" && !seen[i] {
			return invalid("context", "the task's item %d isn't listed", i)
		}
	}
	codex := t.Spec.Harness != nil && t.Spec.Harness.Resolved != nil && t.Spec.Harness.Resolved.Name == "codex"
	if codex != (m.Codex != nil) {
		return invalid("codex", "is set exactly when the task's harness is codex")
	}
	return nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return err
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%q is not an http or https URL", s)
	case u.Host == "":
		return fmt.Errorf("%q has no host", s)
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return fmt.Errorf("%q has user info, a query or a fragment", s)
	}
	return nil
}

// checkPath refuses a path that could leave the bundle: absolute, unclean, with "..", or
// with a backslash. "" is refused only when required.
func checkPath(field, p string, required bool) error {
	switch {
	case p == "":
		if required {
			return invalid(field, "is empty")
		}
		return nil
	case strings.Contains(p, `\`), path.IsAbs(p), path.Clean(p) != p, !filepath.IsLocal(p):
		return invalid(field, "%q is not a clean relative path inside the bundle", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return invalid(field, "%q is not a clean relative path inside the bundle", p)
		}
	}
	return nil
}

// Write lays out the bundle at dir, atomically: it refuses an existing dir, removes a
// leftover dir+".tmp", writes everything there (directories 0700, files 0600, a skill's
// executable files 0700), fsyncs the files and renames it to dir. A failure removes the
// temp dir. Write sets what it lays out in m (Version when 0, the default Dispatcher, the
// file names, Context from the task and f.Context, Codex from f) and refuses what Read would.
func Write(dir string, m Manifest, f Files) (err error) {
	if m.Version == 0 {
		m.Version = Version
	}
	if m.Dispatcher == "" {
		m.Dispatcher = DefaultDispatcherURL
	}
	m.Prompt = PromptFile
	m.OutputSchema, m.Gateway.CAFile = "", ""
	if f.OutputSchema != nil {
		m.OutputSchema = SchemaFile
	}
	if len(f.CA) > 0 {
		m.Gateway.CAFile = CAFile
	}
	m.Codex = f.Codex
	m.Context = nil
	files := map[string][]byte{PromptFile: f.Prompt, m.OutputSchema: f.OutputSchema, m.Gateway.CAFile: f.CA}
	for i, c := range f.Task.Spec.Context {
		if c.Kind == "repo" {
			continue
		}
		raw, ok := f.Context[i]
		if !ok {
			m.Context = append(m.Context, ContextFile{Index: i, Gone: true})
			continue
		}
		name := contextFile(i)
		m.Context = append(m.Context, ContextFile{Index: i, File: name})
		files[name] = raw
	}
	for i := range f.Context {
		if i < 0 || i >= len(f.Task.Spec.Context) || f.Task.Spec.Context[i].Kind == "repo" {
			return invalid("context", "item %d is not a non-repo item of the task", i)
		}
	}
	delete(files, "")
	if err := m.Validate(); err != nil {
		return err
	}
	if err := m.validateTask(f.Task); err != nil {
		return err
	}
	if len(m.Skills) > 0 && f.SkillsDir == "" {
		return invalid("skills", "listed without a skills dir")
	}
	if err := checkSizes(files); err != nil {
		return err
	}
	if files[ManifestFile], err = json.MarshalIndent(m, "", "  "); err != nil {
		return fmt.Errorf("run bundle: %s: %w", ManifestFile, err)
	}
	if files[TaskFile], err = json.MarshalIndent(f.Task, "", "  "); err != nil {
		return fmt.Errorf("run bundle: %s: %w", TaskFile, err)
	}
	if len(files[TaskFile]) > MaxTaskBytes {
		return invalid(TaskFile, "is over %d bytes", MaxTaskBytes)
	}
	if f.Codex != nil {
		if files[CodexFile], err = json.MarshalIndent(f.Codex, "", "  "); err != nil {
			return fmt.Errorf("run bundle: %s: %w", CodexFile, err)
		}
	}

	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("run bundle: %s already exists", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("run bundle: %w", err)
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("run bundle: removing a leftover %s: %w", tmp, err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("run bundle: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	for name, data := range files {
		if err := writeFile(filepath.Join(tmp, filepath.FromSlash(name)), bytes.NewReader(data), 0o600); err != nil {
			return fmt.Errorf("run bundle: %w", err)
		}
	}
	if f.SkillsDir != "" {
		if err := copySkills(f.SkillsDir, filepath.Join(tmp, SkillsDir)); err != nil {
			return fmt.Errorf("run bundle: skills: %w", err)
		}
	}
	for i, s := range m.Skills {
		if fi, err := os.Lstat(filepath.Join(tmp, filepath.FromSlash(s.Dir))); err != nil || !fi.IsDir() {
			return invalid(fmt.Sprintf("skills[%d].dir", i), "%q is not in the skills dir", s.Dir)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("run bundle: %w", err)
	}
	return nil
}

func contextFile(i int) string { return ContextDir + "/" + strconv.Itoa(i) + ".json" }

// checkSizes applies Read's caps to what Write writes, so an oversize item fails on the host,
// before a VM exists. A context file must also hold an apiclient.TaskContext for its index.
func checkSizes(files map[string][]byte) error {
	for name, data := range files {
		limit := int64(MaxContextFile)
		switch name {
		case PromptFile:
			limit = MaxPromptBytes
		case SchemaFile:
			limit = MaxSchemaBytes
		case CAFile:
			limit = MaxCABytes
		}
		if int64(len(data)) > limit {
			return invalid(name, "is over %d bytes", limit)
		}
		if strings.HasPrefix(name, ContextDir+"/") {
			if _, err := parseContext(name, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseContext decodes a context file and checks its index and its content's cap.
func parseContext(name string, data []byte) (apiclient.TaskContext, error) {
	var tc apiclient.TaskContext
	if err := json.Unmarshal(data, &tc); err != nil {
		return tc, invalid(name, "not a task context: %v", err)
	}
	if want := strings.TrimSuffix(path.Base(name), ".json"); strconv.Itoa(tc.Index) != want {
		return tc, invalid(name, "holds item %d", tc.Index)
	}
	if len(tc.Content) > MaxContextBytes {
		return tc, invalid(name, "its content is over %d bytes", MaxContextBytes)
	}
	return tc, nil
}

func writeFile(name string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// copySkills copies the materialised plugin dirs. They have no symlink or special file (the
// pipeline's materialise writes none), so one is refused rather than followed.
func copySkills(src, dst string) error {
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if info.Mode()&0o111 != 0 {
				mode = 0o700
			}
			in, err := root.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = in.Close() }()
			return writeFile(target, in, mode)
		}
		return fmt.Errorf("%s: not a regular file or a directory (%s)", p, d.Type())
	})
}

// Read opens the bundle at dir through an os.Root, reads bundle.json, task.json, the prompt,
// the context files and the schema, each capped, refuses a symlink on the way to any of them,
// and validates the manifest against the task. It doesn't read the skills (they were bounded
// when materialised); it checks each skill's Dir is a directory.
func Read(dir string) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("run bundle: %w", err)
	}
	defer func() { _ = root.Close() }()

	raw, err := readFile(root, ManifestFile, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Dir: dir}
	if err := json.Unmarshal(raw, &b.Manifest); err != nil {
		return nil, invalid(ManifestFile, "%v", err)
	}
	m := b.Manifest
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if raw, err = readFile(root, TaskFile, MaxTaskBytes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &b.Task); err != nil {
		return nil, invalid(TaskFile, "%v", err)
	}
	if err := m.validateTask(b.Task); err != nil {
		return nil, err
	}
	if b.Prompt, err = readFile(root, m.Prompt, MaxPromptBytes); err != nil {
		return nil, err
	}
	b.Context = map[int]apiclient.TaskContext{}
	for _, c := range m.Context {
		if c.Gone {
			continue
		}
		raw, err := readFile(root, c.File, MaxContextFile)
		if err != nil {
			return nil, err
		}
		tc, err := parseContext(contextFile(c.Index), raw)
		if err != nil {
			return nil, err
		}
		b.Context[c.Index] = tc
	}
	if m.OutputSchema != "" {
		if b.OutputSchema, err = readFile(root, m.OutputSchema, MaxSchemaBytes); err != nil {
			return nil, err
		}
	}
	if m.Gateway.CAFile != "" {
		if _, err := readFile(root, m.Gateway.CAFile, MaxCABytes); err != nil {
			return nil, err
		}
		b.CAFile = b.Path(m.Gateway.CAFile)
	}
	for i, s := range m.Skills {
		fi, err := lstat(root, s.Dir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, invalid(fmt.Sprintf("skills[%d].dir", i), "%q is not a directory", s.Dir)
		}
	}
	return b, nil
}

// lstat stats name in root without following a symlink at any of its components.
func lstat(root *os.Root, name string) (fs.FileInfo, error) {
	var fi fs.FileInfo
	segs := strings.Split(name, "/")
	for i := range segs {
		p := strings.Join(segs[:i+1], "/")
		var err error
		if fi, err = root.Lstat(p); err != nil {
			return nil, fmt.Errorf("run bundle: %w", err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, invalid(name, "%s is a symlink", p)
		}
	}
	return fi, nil
}

// readFile reads a regular file of at most limit bytes.
func readFile(root *os.Root, name string, limit int64) ([]byte, error) {
	fi, err := lstat(root, name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, invalid(name, "is not a regular file")
	}
	if fi.Size() > limit {
		return nil, invalid(name, "is over %d bytes", limit)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("run bundle: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("run bundle: %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, invalid(name, "is over %d bytes", limit)
	}
	return data, nil
}

// Path is the absolute path of a bundle-relative path.
func (b *Bundle) Path(rel string) string { return filepath.Join(b.Dir, filepath.FromSlash(rel)) }

// TaskID is the bundle's task id.
func (b *Bundle) TaskID() string { return b.Manifest.TaskID }

// Skills are the manifest's skills with Dir absolute, as harness.Skill has it.
func (b *Bundle) Skills() []Skill {
	out := make([]Skill, len(b.Manifest.Skills))
	for i, s := range b.Manifest.Skills {
		out[i] = Skill{Name: s.Name, Plugin: s.Plugin, Dir: b.Path(s.Dir)}
	}
	return out
}

// ContextItem is the non-repo context item i, as pipeline.PromptSource's Context gives it:
// gone is true when it answered 404. An index the bundle doesn't list is an error.
func (b *Bundle) ContextItem(i int) (c apiclient.TaskContext, gone bool, err error) {
	for _, f := range b.Manifest.Context {
		if f.Index != i {
			continue
		}
		if f.Gone {
			return apiclient.TaskContext{}, true, nil
		}
		return b.Context[i], false, nil
	}
	return apiclient.TaskContext{}, false, fmt.Errorf("run bundle: context item %d is not in the bundle", i)
}
