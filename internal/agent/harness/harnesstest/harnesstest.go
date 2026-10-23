// Package harnesstest has what the harness adapters' tests share: a recording EventSink and
// an end-to-end rig that runs the real run pipeline against an in-process smart-HTTP git
// server and dispatcher. It is imported by tests only.
package harnesstest

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Recorder is a harness.EventSink that records what a run reported. Read its fields once
// the run has returned; safe for concurrent use while it runs.
type Recorder struct {
	mu     sync.Mutex
	Events []harness.ProgressEvent
	Usages []harness.UsageDelta
	Limits []time.Time
	// OnLimit runs on RateLimited (the pipeline cancels the run there).
	OnLimit func()
}

var _ harness.EventSink = (*Recorder)(nil)

// Progress implements harness.EventSink.
func (s *Recorder) Progress(e harness.ProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, e)
}

// Usage implements harness.EventSink.
func (s *Recorder) Usage(u harness.UsageDelta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Usages = append(s.Usages, u)
}

// RateLimited implements harness.EventSink.
func (s *Recorder) RateLimited(until time.Time) {
	s.mu.Lock()
	s.Limits = append(s.Limits, until)
	f := s.OnLimit
	s.mu.Unlock()
	if f != nil {
		f()
	}
}

// Tokens of the end-to-end runs: none may reach a file, a config or a command line.
const (
	GitToken   = "ghs_taskTokenNeverOnDisk0123456789"
	AgentToken = "agent-token-never-on-disk"
)

// E2E is a pipeline run with a real adapter: a smart-HTTP git server with the task repo
// acme/docs (private, GitToken) and acme/ccf-skills (three plugins, two with a "review"
// skill), and a dispatcher serving the prompt and taking the transcript.
type E2E struct {
	t      *testing.T
	git    *httptest.Server
	root   string
	disp   *httptest.Server
	prompt string
	// TaskID is the task's id; WorkRoot holds its workspace (WorkRoot/TaskID).
	TaskID   string
	WorkRoot string
	// SkillsCommit is the ccf-skills commit the task pins.
	SkillsCommit string

	mu          sync.Mutex
	transcripts [][]byte
}

// NewE2E starts the servers. It skips the test when git's http-backend isn't installed.
func NewE2E(t *testing.T, taskID, prompt string) *E2E {
	t.Helper()
	e := &E2E{t: t, prompt: prompt, TaskID: taskID, root: t.TempDir(), WorkRoot: filepath.Join(t.TempDir(), "work")}
	backend := filepath.Join(Git(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + e.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + e.root},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	e.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/acme/docs.git") {
			if u, p, ok := r.BasicAuth(); !ok || u != "x-access-token" || p != GitToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "auth required", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.git.Close)
	e.addRepo("acme/docs", map[string]string{"README.md": "# docs\n", "AGENTS.md": "Docs repo.\n"})
	e.SkillsCommit = e.addRepo("acme/ccf-skills", map[string]string{
		"ccf-docs/.claude-plugin/plugin.json":         `{"name": "ccf-docs"}`,
		"ccf-docs/skills/docs-style/SKILL.md":         "---\nname: docs-style\ndescription: style\n---\n",
		"ccf-docs/skills/docs-style/scripts/check.sh": "#!/bin/sh\necho ok\n",
		"ccf-review/.claude-plugin/plugin.json":       `{"name": "ccf-review"}`,
		"ccf-review/skills/review/SKILL.md":           "---\nname: review\ndescription: review\n---\n",
		"lisa/.claude-plugin/plugin.json":             `{"name": "lisa"}`,
		"lisa/skills/review/SKILL.md":                 "---\nname: review\ndescription: lisa review\n---\n",
	})
	e.disp = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.disp.Close)
	return e
}

// Git runs git in dir with a clean configuration and returns its trimmed output.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	home := t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// addRepo creates owner/name with files on main and returns its head.
func (e *E2E) addRepo(repo string, files map[string]string) string {
	t := e.t
	work := t.TempDir()
	Git(t, work, "init", "--quiet", "-b", "main")
	for name, body := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, work, "add", "-A")
	Git(t, work, "commit", "--quiet", "--no-gpg-sign", "-m", "initial")
	bare := filepath.Join(e.root, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, work, "clone", "--quiet", "--bare", work, bare)
	Git(t, bare, "config", "uploadpack.allowFilter", "true")
	Git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return Git(t, work, "rev-parse", "HEAD")
}

func (e *E2E) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/prompt"):
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, e.prompt)
	case r.Method == http.MethodPut && strings.HasSuffix(p, "/transcript"):
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.transcripts = append(e.transcripts, b)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiclient.TranscriptUpload{TranscriptRef: "blob://transcripts/run_1.jsonl.gz", Bytes: int64(len(b))})
	default: // no output schema (404): the Result schema applies
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"title":"not found"}`)
	}
}

// Pipeline is the run pipeline with adapter a, the test binary as the agent binary (its
// TestMain must answer `fx git-credential`), and an owner environment of PATH, LANG, TERM
// and extra (which the harness must not see).
func (e *E2E) Pipeline(a harness.Adapter, extra ...string) *pipeline.Pipeline {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := pipeline.New(pipeline.Options{
		WorkRoot: e.WorkRoot, Adapters: map[string]harness.Adapter{a.Name(): a}, AgentBinary: exe,
		GitBaseURL: e.git.URL, SkillsCache: filepath.Join(e.t.TempDir(), "cache"),
		Environ: func() []string {
			return append([]string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "TERM=dumb"}, extra...)
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// Job is an implement task on acme/docs (rw, GitToken) with the docs-style skill and both
// review skills, a 10-minute and 20-turn limit, the resolved harness h and outcomes.
func (e *E2E) Job(h v1alpha1.HarnessResolved, outcomes []string) *runner.Job {
	sum := sha256.Sum256([]byte(e.prompt))
	task := v1alpha1.Task{
		Metadata: v1alpha1.TaskMeta{ID: e.TaskID, WorkItem: "wi_1"},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: "implement"},
			Queue:    "poc",
			Target:   &v1alpha1.Target{Provider: "github-public", Repo: "acme/docs"},
			Harness:  &v1alpha1.TaskHarness{Resolved: &h},
			Context:  []v1alpha1.ContextItem{{Kind: "repo", Provider: "github-public", Ref: "acme/docs@main"}},
			Skills:   []v1alpha1.SkillRef{{Source: e.git.URL + "/acme/ccf-skills", Ref: "v0.8.0", Commit: e.SkillsCommit, Use: []string{"docs-style", "ccf-review:review", "lisa:review"}}},
			Prompt:   &v1alpha1.PromptRef{Ref: "api://tasks/" + e.TaskID + "/prompt", Digest: "sha256:" + hex.EncodeToString(sum[:])},
			Limits:   &v1alpha1.Limits{MaxWallClock: "10m", MaxTurns: 20},
			Output:   &v1alpha1.OutputSpec{Outcomes: outcomes},
		},
	}
	run := "run_1"
	cred := apiclient.GitCredential{Provider: "github-public", Host: "github.com", Username: "x-access-token", Token: GitToken,
		Repo: "acme/docs", Permissions: map[string]string{"contents": "write"}, ExpiresAt: time.Now().Add(time.Hour)}
	api, err := apiclient.NewClientWithResponses(e.disp.URL + "/api/v1")
	if err != nil {
		e.t.Fatal(err)
	}
	return &runner.Job{
		Lease:      apiclient.Lease{GitCredential: cred, Lease: apiclient.LeaseInfo{Id: "lse_1", Run: &run, Task: e.TaskID}, Task: task},
		Git:        runner.NewCredentials(cred),
		API:        api,
		Dispatcher: e.disp.URL,
		AgentToken: func() string { return AgentToken },
		StartedAt:  time.Now(),
	}
}

// GatewayCA stands in for the gateway CA's public half in a Bundle.
const GatewayCA = "-----BEGIN CERTIFICATE-----\nZmFrZS1nYXRld2F5LWNh\n-----END CERTIFICATE-----\n"

// CodexAccount is the account a Bundle of a codex task names.
var CodexAccount = runbundle.CodexAccount{AccountID: "acct_test", UserID: "user_test", PlanType: "pro"}

// Bundle writes the run bundle of Job(h, outcomes) with pipeline.PrepareBundle, against the
// dispatcher and the git server, into a new directory under the test's TempDir, and returns
// the directory and the job. The bundle has GatewayCA, the default dispatcher URL and, for a
// codex task, CodexAccount.
func (e *E2E) Bundle(h v1alpha1.HarnessResolved, outcomes []string) (string, *runner.Job) {
	e.t.Helper()
	job := e.Job(h, outcomes)
	x := pipeline.BundleExtras{CA: []byte(GatewayCA)}
	if h.Name == "codex" {
		acct := CodexAccount
		x.Codex = &acct
	}
	dir := filepath.Join(e.t.TempDir(), "bundle")
	if err := e.Pipeline(&fake.Adapter{}).PrepareBundle(context.Background(), job, dir, x); err != nil {
		e.t.Fatalf("PrepareBundle: %v", err)
	}
	return dir, job
}

// Transcript is the uploaded transcript's lines; the run must have uploaded exactly one.
func (e *E2E) Transcript() []map[string]any {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.transcripts) != 1 {
		e.t.Fatalf("%d transcript uploads", len(e.transcripts))
	}
	zr, err := gzip.NewReader(bytes.NewReader(e.transcripts[0]))
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			e.t.Fatalf("transcript line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// TranscriptTypes are the transcript lines' types, in order.
func (e *E2E) TranscriptTypes() []string {
	var types []string
	for _, l := range e.Transcript() {
		if s, ok := l["type"].(string); ok {
			types = append(types, s)
		}
	}
	return types
}

// Sink is a runner.Sink that keeps the progress the worker would send.
type Sink struct {
	mu     sync.Mutex
	events []v1alpha1.ProgressEvent
}

var _ runner.Sink = (*Sink)(nil)

// Progress implements runner.Sink.
func (s *Sink) Progress(e v1alpha1.ProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// Usage implements runner.Sink.
func (*Sink) Usage(runner.UsageDelta) {}

// Events is the progress so far.
func (s *Sink) Events() []v1alpha1.ProgressEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]v1alpha1.ProgressEvent(nil), s.events...)
}
