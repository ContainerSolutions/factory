package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/internal/exectest"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Secrets the tests use: none may reach the disk, a command line, a log or the transcript.
const (
	gitSecret   = "ghs_gitSecretForTheTaskRepo0123456789"
	readSecret  = "ghs_readSecretForThePrivateRepo012345"
	readSecret2 = "ghs_refreshedReadSecretForPrivate6789"
	agentSecret = "agt-token-secret-0123456789abcdef"
)

// secrets are every token the tests hand out.
var secrets = []string{gitSecret, readSecret, readSecret2, agentSecret}

// TestMain doubles as the credential helper: the pipeline runs `<test binary> fx
// git-credential get` through the helper wrapper, exactly as it runs factory-agent.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fx" && os.Args[2] == "git-credential" {
		if err := gitcred.Helper(context.Background(), os.Args[3:], os.Stdin, os.Stdout, os.Getenv(gitcred.EnvSocket)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// testGitEnv isolates the tests' own git commands from the user's config (signing, hooks).
func testGitEnv(home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testGitEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitServer is a smart-HTTP git server (git http-backend through net/http/cgi) with
// repos at /<owner>/<name>.git. A private repo answers 401 without its token.
type gitServer struct {
	t    *testing.T
	root string
	ts   *httptest.Server

	mu     sync.Mutex
	tokens map[string]string // repo → the token it needs; "" = public
	authed map[string]int    // requests that carried the right token, by repo
	denied map[string]int    // requests to a private repo without it, by repo
	sent   map[string]int    // requests to a public repo that carried credentials anyway
	reqs   map[string]int    // every request, by repo
}

func newGitServer(t *testing.T) *gitServer {
	t.Helper()
	execPath := gitIn(t, ".", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	g := &gitServer{t: t, root: t.TempDir(), tokens: map[string]string{}, authed: map[string]int{}, denied: map[string]int{}, sent: map[string]int{}, reqs: map[string]int{}}
	h := &cgi.Handler{
		Path:       backend,
		Env:        []string{"GIT_PROJECT_ROOT=" + g.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + g.root},
		InheritEnv: []string{"PATH"},
		Stderr:     io.Discard,
	}
	g.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := repoOf(r.URL.Path)
		user, pass, hasAuth := r.BasicAuth()
		g.mu.Lock()
		g.reqs[repo]++
		want := g.tokens[repo]
		switch {
		case want == "" && hasAuth:
			g.sent[repo]++
		case want != "" && (!hasAuth || user != "x-access-token" || pass != want):
			g.denied[repo]++
			g.mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		case want != "":
			g.authed[repo]++
		}
		g.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.ts.Close)
	return g
}

// repoOf is "owner/name" from /owner/name.git/…
func repoOf(p string) string {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// addRepo creates owner/name with files on main (and a refs/pull/<n>/head per pulls entry)
// and returns its head commit. A non-empty token makes it private, readable with it.
func (g *gitServer) addRepo(repo string, token string, files map[string]string, pulls map[int]map[string]string) string {
	t := g.t
	t.Helper()
	work := t.TempDir()
	gitIn(t, work, "init", "--quiet", "-b", "main")
	commit := func(files map[string]string, msg string) {
		for name, body := range files {
			p := filepath.Join(work, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o644)
			if strings.HasSuffix(name, ".sh") || strings.Contains("/"+name, "/bin/") {
				mode = 0o755
			}
			if err := os.WriteFile(p, []byte(body), mode); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, work, "add", "-A")
		gitIn(t, work, "commit", "--quiet", "--no-gpg-sign", "-m", msg)
	}
	commit(files, "initial")
	head := gitIn(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(g.root, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "clone", "--quiet", "--bare", work, bare)
	gitIn(t, bare, "config", "uploadpack.allowFilter", "true")
	gitIn(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	gitIn(t, bare, "config", "http.receivepack", "false")
	for n, f := range pulls {
		gitIn(t, work, "checkout", "--quiet", "-b", "pr-"+strconv.Itoa(n), head)
		commit(f, "pr "+strconv.Itoa(n))
		sha := gitIn(t, work, "rev-parse", "HEAD")
		gitIn(t, work, "push", "--quiet", bare, "HEAD:refs/pull/"+strconv.Itoa(n)+"/head")
		_ = sha
	}
	g.setToken(repo, token)
	return head
}

// setToken changes the token a repo needs (a refreshed one).
func (g *gitServer) setToken(repo, token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens[repo] = token
}

// counts is one repo's authenticated, refused and credential-carrying-but-public requests.
func (g *gitServer) counts(repo string) (authed, denied, sent int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authed[repo], g.denied[repo], g.sent[repo]
}

// requests is how many requests a repo got, answered or refused.
func (g *gitServer) requests(repo string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reqs[repo]
}

// dispatcher is the agent API the pipeline calls: the prompt, the output schema, the context
// items and the transcript upload.
type dispatcher struct {
	t      *testing.T
	ts     *httptest.Server
	prompt string
	digest string // the Digest header; "" = none
	// schema is the task's output schema; "" = 404 (none). schemaStatus, when set, is the
	// status getTaskOutputSchema fails with.
	schema       string
	schemaStatus int
	items        map[int]apiclient.TaskContext
	gone         map[int]bool
	limit        int64

	mu          sync.Mutex
	transcripts [][]byte
	calls       []string
	auths       []string
}

func newDispatcher(t *testing.T, prompt string) *dispatcher {
	t.Helper()
	d := &dispatcher{t: t, prompt: prompt, items: map[int]apiclient.TaskContext{}, gone: map[int]bool{}, limit: 20 << 20}
	d.ts = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.ts.Close)
	return d
}

func (d *dispatcher) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, r.Method+" "+r.URL.Path)
	d.auths = append(d.auths, r.Header.Get("Authorization"))
	d.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/prompt"):
		if d.digest != "" {
			w.Header().Set("Digest", d.digest)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, d.prompt)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/output-schema"):
		switch {
		case d.schemaStatus != 0:
			problem(w, d.schemaStatus, http.StatusText(d.schemaStatus))
		case d.schema == "":
			problem(w, http.StatusNotFound, "no output schema")
		default:
			w.Header().Set("Content-Type", "application/schema+json")
			_, _ = io.WriteString(w, d.schema)
		}
	case r.Method == http.MethodGet && strings.Contains(p, "/context/"):
		n, _ := strconv.Atoi(p[strings.LastIndex(p, "/")+1:])
		it, ok := d.items[n]
		if d.gone[n] || !ok {
			problem(w, http.StatusNotFound, "gone")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(it)
	case r.Method == http.MethodPut && strings.HasSuffix(p, "/transcript"):
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/gzip" {
			problem(w, http.StatusUnsupportedMediaType, "not gzip")
			return
		}
		if int64(len(b)) > d.limit {
			problem(w, http.StatusRequestEntityTooLarge, "too large")
			return
		}
		d.mu.Lock()
		d.transcripts = append(d.transcripts, b)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiclient.TranscriptUpload{TranscriptRef: "blob://transcripts/run_1.jsonl.gz", Bytes: int64(len(b))})
	default:
		problem(w, http.StatusNotFound, "no route "+r.Method+" "+p)
	}
}

func problem(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": title})
}

func (d *dispatcher) client(t *testing.T) *apiclient.ClientWithResponses {
	t.Helper()
	c, err := apiclient.NewClientWithResponses(d.ts.URL+"/api/v1", apiclient.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+agentSecret)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (d *dispatcher) uploaded() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.transcripts...)
}

// sink records what the run reported.
type sink struct {
	mu       sync.Mutex
	progress []v1alpha1.ProgressEvent
	usage    []runner.UsageDelta
}

func (s *sink) Progress(e v1alpha1.ProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, e)
}

func (s *sink) Usage(u runner.UsageDelta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = append(s.usage, u)
}

func (s *sink) events() []v1alpha1.ProgressEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]v1alpha1.ProgressEvent(nil), s.progress...)
}

// wrapper writes an executable script that appends its arguments to log and then runs
// target with prefix arguments: it shows what each git and helper invocation's command
// line was.
func wrapper(t *testing.T, dir, name, log, target string, prefix ...string) string {
	t.Helper()
	q := func(s string) string { return gitcred.ShellQuote(s) }
	args := make([]string, len(prefix))
	for i, a := range prefix {
		args[i] = q(a)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> " + q(log) + "\nexec " + q(target) + " " + strings.Join(args, " ") + " \"$@\"\n"
	p := filepath.Join(dir, name)
	exectest.Write(t, p, script, 0o700)
	return p
}
