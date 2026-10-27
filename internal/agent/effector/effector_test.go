package effector_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	taskID     = "tsk_1"
	agentToken = "agent-token-never-in-argv"
)

func TestMain(m *testing.M) {
	// `fx <op>`: the test binary is the run's `factory` CLI, as cmd/factory-agent's fx is, so
	// a test can run an op in a process of its own environment (workspace_test.go).
	if len(os.Args) > 2 && os.Args[1] == "fx" && effector.IsOp(os.Args[2:]) {
		os.Exit(fxMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fxMain is cmd/factory-agent's `fx <op>`: the Effector from this process's environment.
func fxMain(args []string) int {
	e, err := effector.FromEnv(os.Getenv, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "factory:", err)
		return effector.ExitUsage
	}
	return effector.Main(context.Background(), e, args, os.Stdin, os.Stdout, os.Stderr)
}

// dispatcher is an httptest Effector API: it records every request and answers like the
// real one (a known key replays the stored effect), unless answer says otherwise.
type dispatcher struct {
	t  *testing.T
	ts *httptest.Server

	mu      sync.Mutex
	reqs    []recorded
	effects map[string]string
	// answer, when set, answers instead (status, headers, body); status 0 falls through.
	answer func(n int, req map[string]any) (int, http.Header, string)
	// drop, when set, closes the connection without an answer for request n (1-based).
	drop func(n int) bool
}

type recorded struct {
	Path, Auth string
	Body       map[string]any
}

func newDispatcher(t *testing.T) *dispatcher {
	d := &dispatcher{t: t, effects: map[string]string{}}
	d.ts = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.ts.Close)
	return d
}

func (d *dispatcher) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		http.Error(w, "not json", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.reqs = append(d.reqs, recorded{Path: r.Method + " " + r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
	n := len(d.reqs)
	answer, drop := d.answer, d.drop
	d.mu.Unlock()
	if drop != nil && drop(n) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			d.t.Error("no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err == nil {
			_ = c.Close()
		}
		return
	}
	if answer != nil {
		if status, h, text := answer(n, body); status != 0 {
			for k, v := range h {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, text)
			return
		}
	}
	key, _ := body["idempotencyKey"].(string)
	d.mu.Lock()
	id, ok := d.effects[key]
	if !ok {
		sum := sha256.Sum256([]byte(taskID + "\x00" + key))
		id = "eff_" + hex.EncodeToString(sum[:20])
		d.effects[key] = id
	}
	d.mu.Unlock()
	out := map[string]any{"effectId": id}
	switch body["op"] {
	case "create-issue":
		out["url"] = "https://github.com/acme/docs/issues/43"
	case "pr.open":
		out["url"] = "https://github.com/acme/docs/pull/9"
	case "comment", "pr.comment":
		out["url"] = "https://github.com/acme/docs/issues/42#issuecomment-1"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (d *dispatcher) requests() []recorded {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reqs)
}

func (d *dispatcher) keys() []string {
	var out []string
	for _, r := range d.requests() {
		k, _ := r.Body["idempotencyKey"].(string)
		out = append(out, k)
	}
	return out
}

// allOps is an EffectorConfig allowing every op.
func allOps() []string {
	var out []string
	for _, op := range v1alpha1.EffectOps() {
		out = append(out, string(op))
	}
	return out
}

// run is one Effector process environment: the variables a run sets.
type run struct {
	t   *testing.T
	d   *dispatcher
	env map[string]string
	cfg pipeline.EffectorConfig
	dir string
}

func newRun(t *testing.T, d *dispatcher, mut func(*pipeline.EffectorConfig)) *run {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := pipeline.EffectorConfig{Task: taskID, TaskSpec: "plan-epic", Provider: "github-public", Repo: "acme/docs",
		Allow: allOps(), Labels: []string{"triage:*", "docs"}, Outcomes: []string{"decomposed", "single"}}
	if mut != nil {
		mut(&cfg)
	}
	r := &run{t: t, d: d, cfg: cfg, dir: dir, env: map[string]string{
		pipeline.EnvTaskID: taskID, pipeline.EnvRuntime: pipeline.RuntimeWorkspace,
		pipeline.EnvOut: out, pipeline.EnvResult: filepath.Join(out, "result.json"),
	}}
	if d != nil {
		r.env[pipeline.EnvAPI] = d.ts.URL
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, pipeline.EffectorFile)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.env[pipeline.EnvEffector] = p
	return r
}

func (r *run) effector() *effector.Effector {
	r.t.Helper()
	e, err := effector.FromEnv(func(k string) string { return r.env[k] }, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// cli runs `factory <argv>` in a fresh process-like Effector and returns the exit code,
// stdout and stderr.
func (r *run) cli(argv ...string) (int, string, string) {
	return r.cliIn("", argv...)
}

func (r *run) cliIn(stdin string, argv ...string) (int, string, string) {
	r.t.Helper()
	var out, errb bytes.Buffer
	code := effector.Main(context.Background(), r.effector(), argv, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("stdout isn't JSON: %v\n%s", err, s)
	}
	return m
}

// apiCases exercise every API-side op from the CLI.
func apiCases(t *testing.T) []struct {
	name string
	argv []string
} {
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	comments := filepath.Join(dir, "comments.json")
	if err := os.WriteFile(body, []byte("## Plan\n\nTwo sub-issues.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comments, []byte(`[{"path": "docs/intro.md", "line": 12, "body": "This link is dead."}, {"path": "docs/a.md", "line": 3, "startLine": 1, "body": "Typo.", "side": "RIGHT"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	return []struct {
		name string
		argv []string
	}{
		{"comment", []string{"comment", "--body", "Planning the epic."}},
		{"comment with a target, a body file and a key", []string{"comment", "--target", "#43", "--body-file", body, "--key", "plan-summary"}},
		{"pr comment", []string{"pr", "comment", "--pr", "7", "--body", "Rebased on main."}},
		{"label add", []string{"label", "add", "--name", "triage:bug", "--name", "docs"}},
		{"label remove", []string{"label", "remove", "--name", "triage:bug", "--target", "acme/docs#42"}},
		{"issue create", []string{"issue", "create", "--title", "Translate the intro", "--body-file", body, "--labels", "docs,triage:todo"}},
		{"issue sub-add", []string{"issue", "sub-add", "--parent", "42", "--child", "#43"}},
		{"issue close", []string{"issue", "close", "--reason", "not_planned"}},
		{"issue link-duplicate", []string{"issue", "link-duplicate", "--of", "acme/docs#40", "--target", "44"}},
		{"pr open", []string{"pr", "open", "--head", "factory/tsk_1", "--base", "main", "--title", "docs: translate the intro", "--body", "Closes #43.", "--draft", "--labels", "docs"}},
		{"pr update", []string{"pr", "update", "--pr", "https://github.com/acme/docs/pull/9", "--title", "docs: the intro"}},
		{"pr retarget", []string{"pr", "retarget", "--pr", "9", "--base", "release-1"}},
		{"pr merge", []string{"pr", "merge", "--pr", "9", "--sha", "4b825dc", "--merge-label", "triage:merge"}},
		{"review approve", []string{"review", "--pr", "9", "--event", "approve"}},
		{"review request-changes", []string{"review", "--pr", "9", "--event", "request-changes", "--body", "Two problems.", "--comments-file", comments}},
		{"review comment", []string{"review", "--pr", "9", "--event", "comment", "--body", "A nit.", "--comments", `[{"path": "README.md", "line": 1, "body": "nit"}]`, "--commit", "4b825dc"}},
		{"thread reply", []string{"thread", "reply", "--thread", "PRRT_kwDOA", "--body", "Fixed in 4b825dc."}},
		{"thread resolve", []string{"thread", "resolve", "--thread", "PRRT_kwDOA", "--pr", "9"}},
	}
}

var keyRE = regexp.MustCompile(`^tsk_1/[a-z.-]+/[0-9a-f]{16}\.0$`)

// TestAPIOps runs every API-side op through the CLI against an httptest dispatcher: the
// request (golden), the agent token, the key, the output.
func TestAPIOps(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, nil)
	cases := apiCases(t)
	golden := map[string]any{}
	sent := map[string]bool{}
	for _, c := range cases {
		code, stdout, stderr := r.cli(c.argv...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", c.name, code, stdout, stderr)
		}
		reqs := d.requests()
		last := reqs[len(reqs)-1]
		if last.Path != "POST /api/v1/tasks/tsk_1/effects" || last.Auth != "" {
			t.Errorf("%s: %s with %q", c.name, last.Path, last.Auth)
		}
		key, _ := last.Body["idempotencyKey"].(string)
		if !keyRE.MatchString(key) && key != "tsk_1/comment/plan-summary" {
			t.Errorf("%s: key %q", c.name, key)
		}
		golden[c.name] = last.Body
		sent[last.Body["op"].(string)] = true
		out := decode(t, stdout)
		if out["effectId"] == "" || out["idempotencyKey"] != key {
			t.Errorf("%s: output %v", c.name, out)
		}
		switch c.name {
		case "issue create":
			if out["ref"] != "acme/docs#43" || out["url"] != "https://github.com/acme/docs/issues/43" {
				t.Errorf("issue create printed %v", out)
			}
		case "pr open":
			if out["ref"] != "acme/docs#9" {
				t.Errorf("pr open printed %v", out)
			}
		}
	}
	// Every API-side effect op was sent, and every API-side CLI op has a case.
	for _, op := range v1alpha1.EffectOps() {
		if side, _ := op.Side(); side == v1alpha1.SideAPI && !sent[string(op)] {
			t.Errorf("no case sends %s", op)
		}
	}
	for _, o := range effector.Ops() {
		if len(o.Effects) == 0 {
			continue
		}
		if side, _ := o.Effects[0].Side(); side != v1alpha1.SideAPI {
			continue
		}
		if !slices.ContainsFunc(cases, func(c struct {
			name string
			argv []string
		}) bool {
			return strings.HasPrefix(strings.Join(c.argv, " "), strings.Join(o.Words, " ")+" ")
		}) {
			t.Errorf("no case for %s", o.Command())
		}
	}
	checkGolden(t, "requests.golden.json", golden)
}

func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run with -update):\n%s", p, got)
	}
}

// TestIdempotencyKeys: a call that succeeded and is sent again is a new effect; a call
// that failed is sent again with the same key; --key is stable; the counts survive the
// process (the state file) and concurrent callers.
func TestIdempotencyKeys(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, nil)
	for range 2 {
		if code, _, stderr := r.cli("comment", "--body", "same"); code != 0 {
			t.Fatal(stderr)
		}
	}
	keys := d.keys()
	if len(keys) != 2 || keys[0] == keys[1] || !strings.HasSuffix(keys[0], ".0") || !strings.HasSuffix(keys[1], ".1") ||
		strings.TrimSuffix(keys[0], ".0") != strings.TrimSuffix(keys[1], ".1") {
		t.Errorf("two successful calls: %q", keys)
	}
	// A refused call is sent again with the same key, and the next success uses it too.
	d.mu.Lock()
	d.answer = func(n int, _ map[string]any) (int, http.Header, string) {
		if n == 3 {
			return http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}},
				`{"status": 429, "title": "Too Many Requests", "code": "provider-retryable", "detail": "the provider is rate limiting"}`
		}
		return 0, nil, ""
	}
	d.mu.Unlock()
	code, stdout, stderr := r.cli("comment", "--body", "other")
	if code != effector.ExitRejected || !strings.Contains(stderr, "retry in 7s with the same arguments") {
		t.Errorf("429: %d %q", code, stderr)
	}
	if p := decode(t, stdout)["error"].(map[string]any)["problem"].(map[string]any); p["retryAfterSeconds"] != float64(7) || p["code"] != "provider-retryable" {
		t.Errorf("429 problem %v", p)
	}
	if code, _, stderr := r.cli("comment", "--body", "other"); code != 0 {
		t.Fatal(stderr)
	}
	keys = d.keys()
	if keys[2] != keys[3] || !strings.HasSuffix(keys[3], ".0") {
		t.Errorf("a retried call after a 429: %q", keys[2:])
	}
	// --key: the same key every time.
	for range 2 {
		if code, _, stderr := r.cli("comment", "--body", "summary v2", "--key", "plan-summary"); code != 0 {
			t.Fatal(stderr)
		}
	}
	keys = d.keys()
	if keys[4] != "tsk_1/comment/plan-summary" || keys[5] != keys[4] {
		t.Errorf("--key: %q", keys[4:])
	}
	if code, _, _ := r.cli("comment", "--body", "x", "--key", "has space"); code != effector.ExitUsage {
		t.Errorf("a key with a space = %d", code)
	}
	// The state file holds the counts: another process continues them.
	b, err := os.ReadFile(filepath.Join(r.env[pipeline.EnvOut], effector.StateFile))
	if err != nil || !strings.Contains(string(b), `"done"`) {
		t.Fatalf("state %v %s", err, b)
	}
	if code, _, _ := r.cli("comment", "--body", "same"); code != 0 {
		t.Fatal("third")
	}
	if k := d.keys()[6]; k != strings.TrimSuffix(keys[0], ".0")+".2" {
		t.Errorf("the third same call %q", k)
	}
	// Concurrent callers (one MCP server, several CLI processes) don't lose counts.
	var wg sync.WaitGroup
	e := r.effector()
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Do(context.Background(), "comment", map[string]any{"body": "parallel"}); err != nil {
				t.Error(err)
			}
			if code, _, stderr := r.cli("comment", "--body", fmt.Sprintf("cli %d", i)); code != 0 {
				t.Error(stderr)
			}
		}()
	}
	wg.Wait()
	if code, _, _ := r.cli("comment", "--body", "parallel"); code != 0 {
		t.Fatal("after")
	}
	last := d.keys()[len(d.keys())-1]
	if n := last[strings.LastIndex(last, ".")+1:]; n == "0" {
		t.Errorf("the parallel calls' successes weren't counted: %q", last)
	}
}

// TestNetworkRetry: a network error is retried with the same key, so the dispatcher applies
// the effect once even when it got the first request.
func TestNetworkRetry(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	d.drop = func(n int) bool { return n == 1 }
	r := newRun(t, d, nil)
	var slept []time.Duration
	var mu sync.Mutex
	e := effector.New(effector.Options{API: client(t, d.ts.URL), TaskID: taskID, State: filepath.Join(t.TempDir(), "state"),
		Sleep: func(_ context.Context, dur time.Duration) error {
			mu.Lock()
			slept = append(slept, dur)
			mu.Unlock()
			return nil
		}})
	out, err := e.Do(context.Background(), "comment", map[string]any{"body": "once"})
	if err != nil {
		t.Fatal(err)
	}
	keys := d.keys()
	if len(keys) != 2 || keys[0] != keys[1] || out["idempotencyKey"] != keys[0] || len(slept) != 1 {
		t.Errorf("retry: keys %q, slept %v, out %v", keys, slept, out)
	}
	// Unreachable: three attempts, then exit 4 with the key.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	e = effector.New(effector.Options{API: client(t, dead.URL), TaskID: taskID, Sleep: func(context.Context, time.Duration) error { return nil }})
	_, err = e.Do(context.Background(), "label_add", map[string]any{"labels": []any{"docs"}})
	var fe *effector.Error
	if !errors.As(err, &fe) || fe.Exit != effector.ExitRejected || !strings.Contains(fe.Msg, "after 3 attempts") {
		t.Errorf("unreachable: %v", err)
	}
	_ = r
}

func client(t *testing.T, base string) *apiclient.ClientWithResponses {
	t.Helper()
	c, err := apiclient.NewClientWithResponses(base + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestPrecheck: the local refusals (exit 3, nothing sent), and the dispatcher's own
// refusal when there is no local policy (exit 4).
func TestPrecheck(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, func(c *pipeline.EffectorConfig) {
		c.Allow = []string{"comment", "label.add", "review.comment", "create-issue", "pr.open", "pr.merge"}
		c.Labels = []string{"triage:*"}
	})
	tests := []struct {
		argv []string
		code int
		msg  string
	}{
		{[]string{"thread", "resolve", "--thread", "T"}, 3, "thread.resolve is not allowed for this task (effects.allow: comment, label.add, review.comment, create-issue, pr.open, pr.merge)"},
		{[]string{"review", "--pr", "9", "--event", "approve"}, 3, "review.approve is not allowed"},
		{[]string{"label", "add", "--name", "triage:bug", "--name", "wontfix"}, 3, `label "wontfix" (labels) is not allowed for this task (effects.labels: triage:*)`},
		{[]string{"issue", "create", "--title", "t", "--labels", "triage:bug,wontfix"}, 3, `label "wontfix" (labels) is not allowed`},
		{[]string{"pr", "open", "--head", "h", "--base", "main", "--title", "t", "--labels", "docs"}, 3, `label "docs" (labels) is not allowed`},
		{[]string{"pr", "merge", "--pr", "9", "--merge-label", "merge-me"}, 3, `label "merge-me" (mergeLabel) is not allowed`},
		{[]string{"issue", "create", "--title", "t", "--labels", "triage:bug"}, 0, ""},
		{[]string{"issue", "create", "--title", "no labels"}, 0, ""},
		{[]string{"pr", "open", "--head", "h", "--base", "main", "--title", "t", "--labels", "triage:docs"}, 0, ""},
		{[]string{"pr", "merge", "--pr", "9", "--merge-label", "triage:merge"}, 0, ""},
		{[]string{"pr", "merge", "--pr", "9"}, 0, ""},
		{[]string{"label", "remove", "--name", "triage:bug"}, 3, "label.remove is not allowed"},
		{[]string{"push"}, 3, "push-branch is not allowed"},
		{[]string{"stack", "push"}, 3, "stack.push is not allowed"},
		{[]string{"stack", "rebase"}, 3, "stack.rebase is not allowed"},
		{[]string{"label", "add", "--name", "triage:bug"}, 0, ""},
		{[]string{"review", "--pr", "9", "--event", "comment", "--body", "hm"}, 0, ""},
	}
	for _, tt := range tests {
		code, _, stderr := r.cli(tt.argv...)
		if code != tt.code || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%v = %d %q, want %d %q", tt.argv, code, stderr, tt.code, tt.msg)
		}
	}
	if n := len(d.requests()); n != 7 {
		t.Errorf("%d requests, want only the seven allowed", n)
	}
	// No labels allowed at all.
	r = newRun(t, d, func(c *pipeline.EffectorConfig) { c.Labels = nil })
	if code, _, stderr := r.cli("label", "add", "--name", "docs"); code != 3 || !strings.Contains(stderr, "effects.labels: none") {
		t.Errorf("no globs: %d %q", code, stderr)
	}
	// Without a local policy the dispatcher decides: its 403 is exit 4 with its detail.
	d.answer = func(int, map[string]any) (int, http.Header, string) {
		return http.StatusForbidden, nil, `{"status": 403, "title": "Forbidden", "code": "effect-not-allowed", "detail": "op pr.merge is not in the task's effects.allow"}`
	}
	r = newRun(t, d, nil)
	delete(r.env, pipeline.EnvEffector)
	code, stdout, stderr := r.cli("pr", "merge", "--pr", "acme/docs#9")
	if code != 4 || !strings.Contains(stderr, "pr.merge is not allowed: op pr.merge is not in the task's effects.allow") {
		t.Errorf("server 403: %d %q", code, stderr)
	}
	if p := decode(t, stdout)["error"].(map[string]any)["problem"].(map[string]any); p["code"] != "effect-not-allowed" || p["status"] != float64(403) {
		t.Errorf("problem %v", p)
	}
}

// TestDispatcherErrors: each refusal maps to a clear message and exit 4.
func TestDispatcherErrors(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, nil)
	tests := []struct {
		status int
		body   string
		msg    string
	}{
		{400, `{"status": 400, "title": "Bad Request", "code": "invalid", "detail": "the provider refused the effect", "errors": [{"field": "args.title", "message": "required"}]}`,
			"comment was refused as invalid: the provider refused the effect (args.title: required)"},
		{401, `{"status": 401, "title": "Unauthorized"}`, "the dispatcher refused the run's credentials"},
		{403, `{"status": 403, "title": "Forbidden", "code": "not-lease-holder", "detail": "the agent doesn't hold the task's lease"}`, "comment is not allowed: the agent doesn't hold"},
		{404, `{"status": 404, "title": "Not Found", "detail": "the effect's target doesn't exist"}`, "comment: not found: the effect's target"},
		{429, `{"status": 429, "title": "Too Many Requests", "code": "rate-limited", "detail": "the run made 20 comments (limit 20)"}`, "rate limited: the run made 20 comments (limit 20); retry later"},
		{502, `{"status": 502, "title": "Bad Gateway", "code": "provider-error"}`, "comment failed: the dispatcher answered 502: Bad Gateway"},
		{500, `not json`, "the dispatcher answered 500: Internal Server Error"},
	}
	for _, tt := range tests {
		d.answer = func(int, map[string]any) (int, http.Header, string) { return tt.status, nil, tt.body }
		code, _, stderr := r.cli("comment", "--body", "x")
		if code != 4 || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%d: %d %q, want %q", tt.status, code, stderr, tt.msg)
		}
	}
}

// TestUsage: usage errors are exit 2 and send nothing.
func TestUsage(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, nil)
	tests := []struct {
		argv []string
		msg  string
	}{
		{nil, "usage: factory <op>"},
		{[]string{"pr"}, `unknown op "pr"`},
		{[]string{"pr", "close"}, `unknown op "pr close"`},
		{[]string{"comment"}, "body (--body or --body-file) is required"},
		{[]string{"comment", "--body", "x", "extra"}, `unexpected argument "extra"`},
		{[]string{"comment", "--bdy", "x"}, "flag provided but not defined: -bdy"},
		{[]string{"comment", "--body", "x", "--body-file", "f"}, "give the value or a file, not both"},
		{[]string{"comment", "--body-file", "/nonexistent/f"}, "no such file"},
		{[]string{"issue", "close", "--reason", "spam"}, "reason must be one of completed, not_planned"},
		{[]string{"review", "--pr", "9", "--event", "request-changes"}, "needs a body"},
		{[]string{"review", "--pr", "9", "--event", "comment", "--body", "x", "--comments", `{"path": "a"}`}, "comments must be a JSON array"},
		{[]string{"review", "--pr", "9", "--event", "comment", "--body", "x", "--comments", `[{"path": "a"}]`}, "comment 0 needs path, line and body"},
		{[]string{"review", "--pr", "9", "--event", "comment", "--body", "x", "--comments", `[{`}, "not JSON"},
		{[]string{"pr", "update", "--pr", "9"}, "give a title or a body"},
		{[]string{"pr", "comment", "--pr", "acme docs", "--body", "x"}, "is not a reference"},
		{[]string{"pr", "open", "--head", "a", "--base", "b", "--title", "t", "--draft=maybe"}, "not a boolean"},
		{[]string{"result"}, "give the Result file"},
	}
	for _, tt := range tests {
		code, _, stderr := r.cli(tt.argv...)
		if code != 2 || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%v = %d %q, want 2 %q", tt.argv, code, stderr, tt.msg)
		}
	}
	if n := len(d.requests()); n != 0 {
		t.Errorf("usage errors sent %d requests", n)
	}
	// Help is exit 0 on stdout.
	for _, argv := range [][]string{{"help"}, {"pr", "open", "-h"}} {
		var out, errb bytes.Buffer
		if code := effector.Main(context.Background(), r.effector(), argv, nil, &out, &errb); code != 0 {
			t.Errorf("%v = %d", argv, code)
		}
	}
	// A bare number needs the task's repo.
	r = newRun(t, d, func(c *pipeline.EffectorConfig) { c.Repo = "" })
	if code, _, stderr := r.cli("pr", "comment", "--pr", "9", "--body", "x"); code != 2 || !strings.Contains(stderr, "use owner/name#N") {
		t.Errorf("bare number without repo: %d %q", code, stderr)
	}
	// The body from stdin.
	r = newRun(t, d, nil)
	if code, _, stderr := r.cliIn("from stdin\n", "comment", "--body-file", "-"); code != 0 {
		t.Fatal(stderr)
	}
	if b := d.requests()[0].Body["args"].(map[string]any)["body"]; b != "from stdin\n" {
		t.Errorf("stdin body %q", b)
	}
	// Outside a task.
	r = newRun(t, nil, nil)
	if code, _, stderr := r.cli("comment", "--body", "x"); code != 2 || !strings.Contains(stderr, "factory runs inside a factory task") {
		t.Errorf("no FACTORY_API: %d %q", code, stderr)
	}
	r.env[pipeline.EnvEffector] = filepath.Join(t.TempDir(), "missing.json")
	if _, err := effector.FromEnv(func(k string) string { return r.env[k] }, nil); err == nil {
		t.Error("a missing config file was accepted")
	}
}

// TestResult: `factory result` validates with the pipeline's check and copies the file.
func TestResult(t *testing.T) {
	t.Parallel()
	r := newRun(t, nil, nil)
	spec := filepath.Join(r.dir, "result.schema.json")
	if err := os.WriteFile(spec, []byte(`{"type": "object", "required": ["data"], "properties": {"data": {"type": "object", "required": ["plan"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.Schema = spec
	b, _ := json.Marshal(r.cfg)
	if err := os.WriteFile(r.env[pipeline.EnvEffector], b, 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(doc string) string {
		p := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	result := r.env[pipeline.EnvResult]
	bad := []struct{ doc, problem string }{
		{`{"status": "done", "outcome": "decomposed"}`, "missing property 'data'"},
		{`{"status": "done", "outcome": "split", "data": {"plan": []}}`, `/outcome: "split" is not one of decomposed, single`},
		{`{"status": "finished", "data": {"plan": []}}`, "/status"},
		{`{"task": "tsk_2", "status": "noop", "data": {"plan": []}}`, `task is "tsk_2"`},
		{`[1]`, "not a JSON object"},
	}
	for _, tt := range bad {
		code, stdout, stderr := r.cli("result", "--file", write(tt.doc))
		if code != 4 || !strings.Contains(stderr, tt.problem) {
			t.Errorf("%s: %d %q", tt.doc, code, stderr)
		}
		if ps := decode(t, stdout)["error"].(map[string]any)["problems"]; ps == nil {
			t.Errorf("%s: no problems in %s", tt.doc, stdout)
		}
		if _, err := os.Stat(result); err == nil {
			t.Fatalf("an invalid result was copied")
		}
	}
	good := `{"status": "done", "outcome": "single", "summary": "one issue", "data": {"plan": []}}`
	code, stdout, stderr := r.cli("result", "--file", write(good))
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	if got, _ := os.ReadFile(result); string(got) != good {
		t.Errorf("copied %q", got)
	}
	if out := decode(t, stdout); out["status"] != "done" || out["outcome"] != "single" || out["result"] != result {
		t.Errorf("output %v", out)
	}
	// $FACTORY_RESULT itself is validated in place; the MCP form takes the object.
	if code, _, stderr := r.cli("result", "--file", result); code != 0 {
		t.Error(stderr)
	}
	out, err := r.effector().Do(context.Background(), "result", map[string]any{"result": map[string]any{"status": "noop", "data": map[string]any{"plan": []any{}}}})
	if err != nil || out["status"] != "noop" {
		t.Errorf("MCP form: %v %v", out, err)
	}
	// Without a spec schema, the Result schema (the file the pipeline writes then) applies.
	gen, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, gen, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := r.cli("result", "--file", write(`{"status": "done", "outcome": "single"}`)); code != 0 {
		t.Errorf("the generic schema: %s", stderr)
	}
}
