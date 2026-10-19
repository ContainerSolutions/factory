package codex

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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Tokens of the end-to-end runs: none may reach a file, the config or a command line.
const (
	e2eGitToken   = "ghs_taskTokenNeverOnDisk0123456789"
	e2eAgentToken = "agent-token-never-on-disk"
	e2eTaskID     = "tsk_01JCODEX"
)

// e2e is a pipeline run with the codex adapter: a smart-HTTP git server with the task repo
// (private) and a ccf-skills repo, and a dispatcher serving the prompt and taking the
// transcript.
type e2e struct {
	t        *testing.T
	git      *httptest.Server
	gitRoot  string
	disp     *httptest.Server
	prompt   string
	commit   string // the skills commit
	workRoot string

	mu          sync.Mutex
	transcripts [][]byte
}

func newE2E(t *testing.T, prompt string) *e2e {
	t.Helper()
	e := &e2e{t: t, prompt: prompt, gitRoot: t.TempDir(), workRoot: filepath.Join(t.TempDir(), "work")}
	execPath := gitOut(t, ".", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + e.gitRoot, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + e.gitRoot},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	e.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/acme/docs.git") {
			if u, p, ok := r.BasicAuth(); !ok || u != "x-access-token" || p != e2eGitToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "auth required", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.git.Close)
	e.addRepo("acme/docs", map[string]string{"README.md": "# docs\n", "AGENTS.md": "Docs repo.\n"})
	e.commit = e.addRepo("acme/ccf-skills", map[string]string{
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

func gitOut(t *testing.T, dir string, args ...string) string {
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
func (e *e2e) addRepo(repo string, files map[string]string) string {
	t := e.t
	work := t.TempDir()
	gitOut(t, work, "init", "--quiet", "-b", "main")
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
	gitOut(t, work, "add", "-A")
	gitOut(t, work, "commit", "--quiet", "--no-gpg-sign", "-m", "initial")
	bare := filepath.Join(e.gitRoot, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "clone", "--quiet", "--bare", work, bare)
	gitOut(t, bare, "config", "uploadpack.allowFilter", "true")
	gitOut(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return gitOut(t, work, "rev-parse", "HEAD")
}

func (e *e2e) serve(w http.ResponseWriter, r *http.Request) {
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

func (e *e2e) pipeline(a harness.Adapter) *pipeline.Pipeline {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := pipeline.New(pipeline.Options{
		WorkRoot: e.workRoot, Adapters: map[string]harness.Adapter{Name: a}, AgentBinary: exe,
		GitBaseURL: e.git.URL, SkillsCache: filepath.Join(e.t.TempDir(), "cache"),
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "TERM=dumb", "OPENAI_API_KEY=sk-owner-never-passed"}
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *e2e) job(model, effort string, outcomes []string) *runner.Job {
	sum := sha256.Sum256([]byte(e.prompt))
	task := v1alpha1.Task{
		Metadata: v1alpha1.TaskMeta{ID: e2eTaskID, WorkItem: "wi_1"},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: "implement"},
			Queue:    "poc",
			Target:   &v1alpha1.Target{Provider: "github-public", Repo: "acme/docs"},
			Harness: &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{
				Name: Name, Version: "0.80.0", Auth: AuthSubscription, Model: model, ReasoningEffort: effort}},
			Context: []v1alpha1.ContextItem{{Kind: "repo", Provider: "github-public", Ref: "acme/docs@main"}},
			Skills:  []v1alpha1.SkillRef{{Source: e.git.URL + "/acme/ccf-skills", Ref: "v0.8.0", Commit: e.commit, Use: []string{"docs-style", "ccf-review:review", "lisa:review"}}},
			Prompt:  &v1alpha1.PromptRef{Ref: "api://tasks/" + e2eTaskID + "/prompt", Digest: "sha256:" + hex.EncodeToString(sum[:])},
			Limits:  &v1alpha1.Limits{MaxWallClock: "10m", MaxTurns: 20},
			Output:  &v1alpha1.OutputSpec{Outcomes: outcomes},
		},
	}
	run := "run_1"
	cred := apiclient.GitCredential{Provider: "github-public", Host: "github.com", Username: "x-access-token", Token: e2eGitToken,
		Repo: "acme/docs", Permissions: map[string]string{"contents": "write"}, ExpiresAt: time.Now().Add(time.Hour)}
	api, err := apiclient.NewClientWithResponses(e.disp.URL + "/api/v1")
	if err != nil {
		e.t.Fatal(err)
	}
	return &runner.Job{
		Lease:      apiclient.Lease{GitCredential: cred, Lease: apiclient.LeaseInfo{Id: "lse_1", Run: &run, Task: e2eTaskID}, Task: task},
		Git:        runner.NewCredentials(cred),
		API:        api,
		Dispatcher: e.disp.URL,
		AgentToken: func() string { return e2eAgentToken },
		StartedAt:  time.Now(),
	}
}

// transcript is the uploaded transcript's lines.
func (e *e2e) transcript() []map[string]any {
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

type nopSink struct {
	mu       sync.Mutex
	progress []v1alpha1.ProgressEvent
}

func (s *nopSink) Progress(e v1alpha1.ProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, e)
}
func (*nopSink) Usage(runner.UsageDelta) {}

// TestPipelineEndToEnd runs the pipeline with the codex adapter on the fake codex: the run
// writes an invalid Result, the repair turn resumes the session and answers a valid one as
// its final message.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	e := newE2E(t, "Open a PR on acme/docs.\n")
	owner := ownerHome(t)
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{
		{Stream: "codex-success.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`},
		{Stream: "codex-success.jsonl", LastMessage: okResult},
	}})
	a := New(Options{Config: config.Harness{Home: owner}, Binary: fc.Binary})
	sink := &nopSink{}
	out, err := e.pipeline(a).Run(context.Background(), e.job("gpt-5-codex", "high", []string{"pr-opened"}), sink)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" {
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	h := out.Result.Harness
	if h == nil || h.Usage.InputTokens != 2*24763 || h.Usage.OutputTokens != 2*1220 || h.Usage.Turns != 2 || h.Usage.CostUSD != nil {
		t.Errorf("result harness %+v", h)
	}

	calls := fc.calls()
	if len(calls) != 2 {
		t.Fatalf("%d codex execs, want the run and the repair", len(calls))
	}
	run, repair := calls[0], calls[1]
	repo := filepath.Join(e.workRoot, e2eTaskID, "repo")
	for _, w := range []string{"--sandbox", "workspace-write", "--cd", repo, "--model", "gpt-5-codex", `model_reasoning_effort="high"`,
		"--output-schema", filepath.Join(e.workRoot, e2eTaskID, "result.schema.json")} {
		if !slices.Contains(run.Args, w) {
			t.Errorf("run args %q lack %q", run.Args, w)
		}
	}
	if !strings.Contains(run.Stdin, "Open a PR on acme/docs.") || !strings.Contains(run.Stdin, "unattended inside CCF Factory") {
		t.Errorf("run prompt %q", run.Stdin)
	}
	if i := slices.Index(repair.Args, "resume"); i < 0 || repair.Args[i+1] != "0199f2a1-6b1e-7c33-9a51-2f0c4e8d1a01" ||
		!strings.Contains(repair.Stdin, "/outcome") {
		t.Errorf("repair args %q, prompt %q", repair.Args, repair.Stdin)
	}
	home := filepath.Join(e.workRoot, e2eTaskID, "home", ".codex")
	if run.Env["CODEX_HOME"] != home || run.Env["OPENAI_API_KEY"] != "" || run.Env["FACTORY_RESULT"] == "" || run.Env["GH_TOKEN"] != e2eGitToken {
		t.Errorf("run env %v", run.Env)
	}
	for _, w := range []string{"auth.json -> " + filepath.Join(owner, "auth.json"), "skills/docs-style/SKILL.md",
		"skills/docs-style/scripts/check.sh", "skills/ccf-review-review/SKILL.md", "skills/lisa-review/SKILL.md", "config.toml", "AGENTS.md"} {
		if !slices.Contains(run.Home, w) {
			t.Errorf("CODEX_HOME during the run lacks %q: %q", w, run.Home)
		}
	}
	if !strings.Contains(run.Agents, "unattended inside CCF Factory (task "+e2eTaskID) || !strings.Contains(run.Config, `"FACTORY_TOKEN"`) {
		t.Errorf("AGENTS.md %q\nconfig.toml %q", run.Agents, run.Config)
	}
	for _, s := range append(testSecrets, e2eGitToken, e2eAgentToken) {
		if strings.Contains(run.Config, s) || strings.Contains(run.Agents, s) || slices.ContainsFunc(run.Args, func(a string) bool { return strings.Contains(a, s) }) {
			t.Errorf("a token is in the config, AGENTS.md or the command line: %q", s)
		}
	}

	var types []string
	for _, l := range e.transcript() {
		if s, ok := l["type"].(string); ok {
			types = append(types, s)
		}
	}
	for _, w := range []string{"factory.codex-exec", "thread.started", "turn.completed", "factory.repair"} {
		if !slices.Contains(types, w) {
			t.Errorf("the transcript lacks a %s line: %v", w, types)
		}
	}
	if len(sink.progress) == 0 {
		t.Error("no progress reached the worker")
	}
	if _, err := os.Stat(filepath.Join(e.workRoot, e2eTaskID)); !os.IsNotExist(err) {
		t.Errorf("the workspace is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(owner, "auth.json")); string(b) != ownerLogin {
		t.Errorf("the owner's login changed or went with the workspace: %q", b)
	}
}

// TestPipelineRateLimited: a usage limit in codex's stream ends the run throttled until the
// limit's reset.
func TestPipelineRateLimited(t *testing.T) {
	t.Parallel()
	e := newE2E(t, "Open a PR.\n")
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{{Stream: "codex-ratelimit.jsonl", Block: true}}})
	before := time.Now()
	a := New(Options{Config: config.Harness{Home: ownerHome(t)}, Binary: fc.Binary, KillGrace: 200 * time.Millisecond})
	out, err := e.pipeline(a).Run(context.Background(), e.job("gpt-5-codex", "low", []string{"pr-opened"}), &nopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || out.ThrottledUntil.Before(before.Add(2*time.Hour)) || out.Result != nil {
		t.Errorf("outcome %+v", out)
	}
}

// TestSmokeCodex is `make smoke-codex`: a trivial task end to end through the real codex,
// on a maintainer machine signed in to a dedicated CODEX_HOME. It never runs in CI or in
// `make test`: it needs FACTORY_SMOKE_CODEX=1.
//
//	FACTORY_SMOKE_CODEX_HOME   the dedicated CODEX_HOME (default ~/.factory-agent/codex-home)
//	FACTORY_SMOKE_CODEX_MODEL  the model (default gpt-5-codex)
//	FACTORY_SMOKE_CODEX_EFFORT the normalised effort (default low)
//	FACTORY_SMOKE_CODEX_BIN    the codex binary (default codex on PATH)
func TestSmokeCodex(t *testing.T) {
	if os.Getenv("FACTORY_SMOKE_CODEX") != "1" || os.Getenv("CI") != "" {
		t.Skip("the smoke run needs a signed-in codex on a maintainer machine: make smoke-codex")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	home, err := config.ExpandPath(env("FACTORY_SMOKE_CODEX_HOME", "~/.factory-agent/codex-home"), os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	model := env("FACTORY_SMOKE_CODEX_MODEL", "gpt-5-codex")
	effort := env("FACTORY_SMOKE_CODEX_EFFORT", "low")
	a := New(Options{Config: config.Harness{Home: home, Default: model}, Binary: env("FACTORY_SMOKE_CODEX_BIN", "codex")})
	off, err := a.Probe(context.Background(), config.Harness{Home: home, Models: []string{model}, Default: model})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probe: %+v", off)

	const want = `{"status": "done", "outcome": "smoke", "summary": "codex smoke test"}`
	e := newE2E(t, "This is a smoke test of CCF Factory's Codex adapter. Don't change any file. "+
		"Run `ls` in the repository, then write exactly this JSON to the file named by $FACTORY_RESULT: "+want+
		" and reply with the same JSON object, and nothing else, as your final message.\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sink := &nopSink{}
	out, err := e.pipeline(a).Run(ctx, e.job(model, effort, []string{"smoke"}), sink)
	lines := e.transcript()
	for _, l := range lines {
		if l["type"] == "factory.codex-exec" || l["type"] == "factory.codex-stderr" {
			t.Logf("%v", l)
		}
	}
	for _, p := range sink.progress {
		t.Logf("progress %s: %s", p.Kind, p.Message)
	}
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "smoke" {
		for _, l := range lines {
			t.Logf("transcript: %v", l)
		}
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	if h := out.Result.Harness; h == nil || h.Usage.Turns < 1 || h.Usage.InputTokens == 0 {
		t.Errorf("usage %+v", h)
	}
	t.Logf("result: %+v, usage %+v", out.Result, out.Result.Harness.Usage)
}
