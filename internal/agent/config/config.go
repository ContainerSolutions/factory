// Package config is factory-agent's local, owner-controlled configuration
// (design/workstreams/agent/LLD/supervise.md §The local config): the dispatcher, the pool, the
// enrollment token file, labels, the harnesses' dedicated config dirs, the owner's lending
// consent, and how the agent polls for tasks.
//
// The file is YAML 1.2 decoded strictly, the way v1alpha1 decodes the catalog: YAML → JSON →
// encoding/json with DisallowUnknownFields, so unknown keys, duplicate keys and non-string keys
// are errors.
package config

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
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// DefaultPath is where factory-agent looks for its config when --config isn't given.
const DefaultPath = "~/.config/factory-agent/agent.yaml"

// DefaultWorkRoot holds the per-task scratch workspaces when workRoot isn't set.
const DefaultWorkRoot = "~/.factory-agent/work"

// The harnesses factory-agent has adapters for (harness-adapters.md).
const (
	HarnessCodex      = "codex"
	HarnessClaudeCode = "claude-code"
)

// Harnesses lists the known harness names.
func Harnesses() []string { return []string{HarnessClaudeCode, HarnessCodex} }

// Efforts are the normalised reasoning efforts a harness may offer (harness-adapters.md's
// effort mappings).
func Efforts() []string { return []string{"minimal", "low", "medium", "high", "max"} }

// maxFileSize bounds the config and token files.
const maxFileSize = 1 << 20

// Config is the worker's local configuration.
type Config struct {
	// Dispatcher is the dispatcher's base URL (the API is under /api/v1). https, or http on a
	// loopback host only: the agent token travels on every call.
	Dispatcher string `json:"dispatcher"`
	// Pool is the registered Pool to join.
	Pool string `json:"pool"`
	// EnrollmentTokenFile holds the one-time enrollment token an admin minted with `factory
	// agent enroll` (P1 §6), 0600. supervise reads it only while its state dir has no refresh
	// token, exchanges it once and re-registers with the refresh token from then on.
	EnrollmentTokenFile string `json:"enrollmentTokenFile,omitzero"`
	// Host names the agent (agt_<owner>-<host>); default: the machine's short host name,
	// lower-cased.
	Host string `json:"host,omitzero"`
	// Labels are the capability labels to advertise.
	Labels []string `json:"labels,omitzero"`
	// Harnesses configures each harness by name (codex, claude-code).
	Harnesses map[string]Harness `json:"harnesses"`
	// WorkRoot holds the per-task scratch workspaces; default ~/.factory-agent/work.
	WorkRoot string `json:"workRoot,omitzero"`
	// Lending is the owner's consent: what the agent may take. Sent at registration and again
	// on SIGHUP.
	Lending v1alpha1.Lending `json:"lending"`
	// Workspace is the VM workspace setup; `factory-agent host` reads only this block.
	Workspace *Workspace `json:"workspace,omitzero"`
	// Poll is how the lease loop asks the dispatcher for tasks; Parse fills its defaults
	// (the long poll).
	Poll Poll `json:"poll,omitzero"`

	// Path is the file the config was read from (not a config key).
	Path string `json:"-"`
}

// Harness is one harness's local config: what the owner's plan allows (harnesses don't
// reliably list it, 10-poc.md §Capabilities). Its login is the one `factory-agent login`
// stored, which the VM workspace's gateway adds.
type Harness struct {
	// Models the owner's plan allows.
	Models []string `json:"models"`
	// Efforts are the normalised reasoning efforts offered.
	Efforts []string `json:"efforts,omitzero"`
	// Default is the model used when a task asks for none; one of Models.
	Default string `json:"default,omitzero"`
}

// The poll block's defaults (the long poll) and bounds (P3 §Agents poll briefly).
const (
	// DefaultPollWait is poll.wait's default, the long poll's.
	DefaultPollWait = "30s"
	// DefaultPollInterval is poll.interval's default: poll again at once after a 204.
	DefaultPollInterval = "0s"
	// MaxPollWait is the longest poll.wait (the dispatcher's cap).
	MaxPollWait = 60 * time.Second
	// MinPollInterval is the shortest poll.interval a wait of 0s may have.
	MinPollInterval = time.Second
)

// Poll is how supervise's lease loop asks the dispatcher for tasks (P3 §Agents poll briefly).
// The defaults keep the long poll; an agent of a dispatcher where idle time must cost nothing
// sets wait: 0s and an interval such as 30s.
type Poll struct {
	// Wait is how long each LeaseTask asks the dispatcher to hold the request: whole seconds
	// from 0s to 60s. Default 30s. A wait of 0s needs an interval of at least 1s.
	Wait string `json:"wait,omitzero"`
	// Interval is how long an idle agent sleeps after a 204 before it polls again; never
	// negative. Default 0s. A poll that gets a task polls again at once.
	Interval string `json:"interval,omitzero"`
}

// WaitDuration is poll.wait; 0 when it isn't valid (Parse refuses it then).
func (p Poll) WaitDuration() time.Duration { d, _ := time.ParseDuration(p.Wait); return d }

// IntervalDuration is poll.interval; 0 when it isn't valid (Parse refuses it then).
func (p Poll) IntervalDuration() time.Duration { d, _ := time.ParseDuration(p.Interval); return d }

// validate checks a poll block with its defaults filled.
func (p Poll) validate() []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf("poll."+format, args...)) }
	wait, werr := time.ParseDuration(p.Wait)
	if werr != nil || wait < 0 || wait > MaxPollWait || wait%time.Second != 0 {
		add("wait: %q must be whole seconds from 0s to %ds", p.Wait, int(MaxPollWait/time.Second))
	}
	interval, ierr := time.ParseDuration(p.Interval)
	switch {
	case ierr != nil:
		add("interval: %q is not a duration (like 30s)", p.Interval)
	case interval < 0:
		add("interval: %q must not be negative", p.Interval)
	case werr == nil && wait == 0 && interval < MinPollInterval:
		add("interval: %q: a wait of 0s needs an interval of at least %s, or the agent polls in a hot loop", p.Interval, MinPollInterval)
	}
	return probs
}

// ValidationError lists every problem found in a config, each prefixed with its key.
type ValidationError struct {
	Path     string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config %s: %s", e.Path, strings.Join(e.Problems, "; "))
}

// Options are Load's injectable dependencies.
type Options struct {
	// Home expands a leading ~/ in paths. Default os.UserHomeDir.
	Home func() (string, error)
	// Hostname names the agent when host isn't set. Default os.Hostname.
	Hostname func() (string, error)
	// Warn gets non-fatal findings (a config readable by others). Default: discarded.
	Warn func(string)
	// GOOS picks the default workspace driver. Default runtime.GOOS.
	GOOS string
}

func (o *Options) defaults() {
	if o.Home == nil {
		o.Home = os.UserHomeDir
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.Warn == nil {
		o.Warn = func(string) {}
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
}

// ExpandPath expands a leading ~ or ~/ with home.
func ExpandPath(p string, home func() (string, error)) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("expand %s: %w", p, err)
	}
	return filepath.Join(h, strings.TrimPrefix(p, "~")), nil
}

// Load reads, decodes and validates the config at path (a leading ~ is expanded). The file
// must not be writable by group or others: whoever can write it can point the worker, and the
// owner's login, at another dispatcher.
func Load(path string, o Options) (*Config, error) {
	o.defaults()
	p, data, err := readConfig(path, o)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, o)
	if err != nil {
		return nil, withPath(err, p)
	}
	cfg.Path = p
	return cfg, nil
}

// LoadWorkspace reads only the workspace block of the config at path, with the defaults
// filled: `factory-agent host` needs no dispatcher, pool or enrollment token. The file is checked
// as Load checks it, and unknown keys are still errors; the other blocks aren't validated.
// A missing file gives the defaults when missingOK is set.
func LoadWorkspace(path string, missingOK bool, o Options) (Workspace, error) {
	o.defaults()
	p, data, err := readConfig(path, o)
	if err != nil {
		if missingOK && errors.Is(err, fs.ErrNotExist) {
			return Workspace{}.WithDefaults(o.GOOS), nil
		}
		return Workspace{}, err
	}
	var cfg Config
	if err := decodeStrict(data, &cfg); err != nil {
		return Workspace{}, fmt.Errorf("config %s: %w", p, err)
	}
	w := Workspace{}
	if cfg.Workspace != nil {
		w = *cfg.Workspace
	}
	w = w.WithDefaults(o.GOOS)
	if probs := w.validate(); len(probs) > 0 {
		return Workspace{}, &ValidationError{Path: p, Problems: probs}
	}
	return w, nil
}

// readConfig expands path and reads the file, refusing it when group or others can write it:
// whoever can write it can point the worker, and the owner's login, at another dispatcher.
func readConfig(path string, o Options) (string, []byte, error) {
	p, err := ExpandPath(path, o.Home)
	if err != nil {
		return "", nil, err
	}
	data, mode, err := readFile(p)
	if err != nil {
		return "", nil, fmt.Errorf("read config: %w", err)
	}
	if mode&0o022 != 0 {
		return "", nil, fmt.Errorf("config %s is writable by group or others (mode %04o): chmod 600 it", p, mode)
	}
	if mode&0o044 != 0 {
		o.Warn(fmt.Sprintf("config %s is readable by group or others (mode %04o); the LLD asks for 0600", p, mode))
	}
	return p, data, nil
}

func withPath(err error, p string) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		ve.Path = p
		return ve
	}
	return fmt.Errorf("config %s: %w", p, err)
}

// Parse decodes and validates a config document, fills the defaults and expands ~ in paths.
func Parse(data []byte, o Options) (*Config, error) {
	o.defaults()
	var cfg Config
	if err := decodeStrict(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.complete(o); err != nil {
		return nil, err
	}
	if cfg.Workspace != nil {
		w := cfg.Workspace.WithDefaults(o.GOOS)
		cfg.Workspace = &w
	}
	if probs := cfg.validate(); len(probs) > 0 {
		return nil, &ValidationError{Problems: probs}
	}
	return &cfg, nil
}

// complete fills the defaults and expands the paths.
func (c *Config) complete(o Options) error {
	if c.WorkRoot == "" {
		c.WorkRoot = DefaultWorkRoot
	}
	if c.Poll.Wait == "" {
		c.Poll.Wait = DefaultPollWait
	}
	if c.Poll.Interval == "" {
		c.Poll.Interval = DefaultPollInterval
	}
	c.Dispatcher = strings.TrimRight(c.Dispatcher, "/")
	if c.Host == "" {
		h, err := o.Hostname()
		if err != nil {
			return fmt.Errorf("host name: %w", err)
		}
		c.Host = HostFrom(h)
	}
	expand := func(p *string) error {
		if *p == "" {
			return nil
		}
		v, err := ExpandPath(*p, o.Home)
		*p = v
		return err
	}
	for _, p := range []*string{&c.EnrollmentTokenFile, &c.WorkRoot} {
		if err := expand(p); err != nil {
			return err
		}
	}
	return nil
}

// hostRE is the dispatcher's rule for the host part of an agent id (scheduler.hostRE).
var hostRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// HostFrom turns a machine host name into a valid agent host: its first label, lower-cased,
// with every character outside [a-z0-9-] replaced by '-'.
func HostFrom(hostname string) string {
	h, _, _ := strings.Cut(strings.ToLower(hostname), ".")
	h = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, h)
	h = strings.TrimLeft(h, "-")
	if h == "" {
		return "agent"
	}
	return h
}

// labelRE is a capability label: lower-case words joined by '-'.
var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (c *Config) validate() []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	if err := checkBaseURL(c.Dispatcher); err != nil {
		add("dispatcher: %v", err)
	}
	if c.Pool == "" {
		add("pool: required")
	}
	if c.EnrollmentTokenFile != "" && !filepath.IsAbs(c.EnrollmentTokenFile) {
		add("enrollmentTokenFile: %q must be absolute or start with ~/", c.EnrollmentTokenFile)
	}
	if !hostRE.MatchString(c.Host) {
		add("host: %q must match %s", c.Host, hostRE)
	}
	seen := map[string]bool{}
	for i, l := range c.Labels {
		switch {
		case !labelRE.MatchString(l):
			add("labels[%d]: %q must be lower-case words joined by '-'", i, l)
		case seen[l]:
			add("labels[%d]: %q is listed twice", i, l)
		}
		seen[l] = true
	}
	if c.WorkRoot != "" && !filepath.IsAbs(c.WorkRoot) {
		add("workRoot: %q must be absolute or start with ~/", c.WorkRoot)
	}
	if len(c.Harnesses) == 0 {
		add("harnesses: at least one of %s", strings.Join(Harnesses(), ", "))
	}
	names := make([]string, 0, len(c.Harnesses))
	for n := range c.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, p := range c.Harnesses[n].validate(n) {
			add("harnesses.%s.%s", n, p)
		}
	}
	for _, p := range validateLending(c.Lending) {
		add("lending%s", p)
	}
	if c.Workspace != nil {
		probs = append(probs, c.Workspace.validate()...)
	}
	probs = append(probs, c.Poll.validate()...)
	return probs
}

func (h Harness) validate(name string) []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	if !slices.Contains(Harnesses(), name) {
		return []string{fmt.Sprintf("unknown harness (known: %s)", strings.Join(Harnesses(), ", "))}
	}
	if len(h.Models) == 0 {
		add("models: at least one model")
	}
	for i, m := range h.Models {
		if strings.TrimSpace(m) == "" || slices.Index(h.Models, m) != i {
			add("models[%d]: %q is empty or listed twice", i, m)
		}
	}
	for i, e := range h.Efforts {
		if !slices.Contains(Efforts(), e) || slices.Index(h.Efforts, e) != i {
			add("efforts[%d]: %q is not one of %s, or is listed twice", i, e, strings.Join(Efforts(), ", "))
		}
	}
	if h.Default != "" && !slices.Contains(h.Models, h.Default) {
		add("default: %q is not one of models", h.Default)
	}
	return probs
}

// validateLending checks the consent block. It must lend something: unlimited, or a list of
// queues (the scheduler lends nothing otherwise); unlimited ignores every list and cap but
// maxConcurrent, so combining them is refused rather than silently ignored.
func validateLending(l v1alpha1.Lending) []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	switch {
	case l.MaxConcurrent < 0:
		add(".maxConcurrent: must not be negative")
	case l.MaxRunsPerDay < 0:
		add(".maxRunsPerDay: must not be negative")
	case l.MaxTokensPerDay < 0:
		add(".maxTokensPerDay: must not be negative")
	}
	if l.Unlimited {
		var extra []string
		if len(l.Queues) > 0 {
			extra = append(extra, "queues")
		}
		if len(l.RuleSets) > 0 {
			extra = append(extra, "ruleSets")
		}
		if len(l.Harnesses) > 0 {
			extra = append(extra, "harnesses")
		}
		if l.Hours != "" {
			extra = append(extra, "hours")
		}
		if l.MaxRunsPerDay > 0 {
			extra = append(extra, "maxRunsPerDay")
		}
		if l.MaxTokensPerDay > 0 {
			extra = append(extra, "maxTokensPerDay")
		}
		if len(extra) > 0 {
			add(": unlimited lends everything; drop %s (only maxConcurrent applies)", strings.Join(extra, ", "))
		}
	} else if len(l.Queues) == 0 {
		add(": set unlimited: true or list the queues to lend to")
	}
	for i, h := range l.Harnesses {
		if !slices.Contains(Harnesses(), h) {
			add(".harnesses[%d]: %q is not one of %s", i, h, strings.Join(Harnesses(), ", "))
		}
	}
	return probs
}

// MaxConcurrent is how many tasks the worker runs at once: lending.maxConcurrent, default 1.
func (c *Config) MaxConcurrent() int {
	if c.Lending.MaxConcurrent > 0 {
		return c.Lending.MaxConcurrent
	}
	return 1
}

// checkBaseURL accepts an absolute http(s) URL without credentials, query or fragment; plain
// http only on a loopback host.
func checkBaseURL(raw string) error {
	if raw == "" {
		return errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL", raw)
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "http", u.Host == "":
		return fmt.Errorf("%q must be an absolute http(s) URL", raw)
	case u.User != nil, u.RawQuery != "", u.Fragment != "":
		return fmt.Errorf("%q must not carry credentials, a query or a fragment", raw)
	case u.Scheme == "http" && !isLoopback(u.Hostname()):
		return fmt.Errorf("%q: plain http is only allowed on a loopback host (tokens travel on every call)", raw)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ReadSecretFile reads a token file (the enrollment or refresh token): it must be a regular file, not readable
// or writable by group or others, and non-empty once trimmed. Errors never carry the content.
func ReadSecretFile(path string) (string, error) {
	data, mode, err := readFile(path)
	if err != nil {
		return "", err
	}
	if mode&0o077 != 0 {
		return "", fmt.Errorf("%s is accessible to group or others (mode %04o): chmod 600 it", path, mode)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	if strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("%s must hold a single line", path)
	}
	return tok, nil
}

// readFile reads a regular file of at most maxFileSize and returns its permission bits.
func readFile(path string) ([]byte, fs.FileMode, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxFileSize {
		return nil, 0, fmt.Errorf("%s is larger than %d bytes", path, maxFileSize)
	}
	return data, st.Mode().Perm(), nil
}

// decodeStrict decodes one YAML 1.2 document into v: YAML node → generic value → JSON →
// encoding/json with DisallowUnknownFields (the v1alpha1 decoder's approach, so `on`, `yes`
// and `no` are strings, and duplicate or non-string keys are errors).
func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty config")
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return errors.New("want exactly one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	if len(doc.Content) == 0 {
		return errors.New("empty config")
	}
	budget := maxNodes
	val, err := yamlValue(doc.Content[0], &budget)
	if err != nil {
		return err
	}
	if _, ok := val.(map[string]any); !ok {
		return errors.New("the config must be a mapping")
	}
	j, err := json.Marshal(val)
	if err != nil {
		return err
	}
	jd := json.NewDecoder(bytes.NewReader(j))
	jd.DisallowUnknownFields()
	if err := jd.Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// maxNodes bounds alias expansion.
const maxNodes = 1 << 16

func yamlValue(n *yaml.Node, budget *int) (any, error) {
	if *budget--; *budget < 0 {
		return nil, errors.New("document too large (alias expansion)")
	}
	switch n.Kind {
	case yaml.AliasNode:
		return yamlValue(n.Alias, budget)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		lines := make(map[string]int, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			for k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				if k.ShortTag() == "!!merge" {
					return nil, fmt.Errorf("line %d: merge keys (<<) are not supported", k.Line)
				}
				return nil, fmt.Errorf("line %d: mapping key %q (%s) is not a string; quote it", k.Line, k.Value, k.ShortTag())
			}
			if first, dup := lines[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q already defined at line %d", k.Line, k.Value, first)
			}
			lines[k.Value] = k.Line
			v, err := yamlValue(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		l := make([]any, len(n.Content))
		for i, c := range n.Content {
			v, err := yamlValue(c, budget)
			if err != nil {
				return nil, err
			}
			l[i] = v
		}
		return l, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str", "!!timestamp":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		default:
			return nil, fmt.Errorf("line %d: unsupported tag %s", n.Line, n.ShortTag())
		}
	default:
		return nil, fmt.Errorf("line %d: unexpected YAML node", n.Line)
	}
}
