package effector_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/gitsafe/gitsafetest"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
)

const gitToken = "ghs_scopedTaskTokenNeverInArgv0123"

// testGit runs git with a clean config and returns its trimmed output.
func testGit(t *testing.T, dir string, args ...string) string {
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

// workspace is a run's checkout of the private repo acme/app, served over smart HTTP (git
// http-backend) that needs the task token, reached only through the credential helper.
type workspace struct {
	t      *testing.T
	bare   string
	repo   string
	env    []string
	argv   string // the git wrapper's log: one line of arguments, then GH_TOKEN's presence
	gitBin string // the git wrapper
	server *httptest.Server
	run    *run
	d      *dispatcher
}

func newWorkspace(t *testing.T, mut func(*pipeline.EffectorConfig)) *workspace {
	t.Helper()
	backend := filepath.Join(testGit(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	root := t.TempDir()
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "x-access-token" || p != gitToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	// The bare repo: main with one commit.
	seed := t.TempDir()
	testGit(t, seed, "init", "--quiet", "-b", "main")
	writeFile(t, filepath.Join(seed, "README.md"), "# app\n")
	testGit(t, seed, "add", "-A")
	testGit(t, seed, "commit", "--quiet", "--no-gpg-sign", "-m", "initial")
	bare := filepath.Join(root, "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, seed, "clone", "--quiet", "--bare", seed, bare)
	testGit(t, bare, "config", "http.receivepack", "true")

	// The credential helper, answering for acme/app only, from memory.
	u, _ := url.Parse(srv.URL)
	creds, err := gitcred.Listen("", func() []gitcred.Entry {
		return []gitcred.Entry{{Protocol: "http", Host: u.Host, Repo: "acme/app", Username: "x-access-token", Password: gitToken}}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = creds.Close() })
	home := t.TempDir()
	w := &workspace{t: t, bare: bare, server: srv, argv: filepath.Join(t.TempDir(), "git-argv.log")}
	// A git wrapper that logs each command line (and whether it got GH_TOKEN) before
	// running git, so the test can check no token is ever an argument.
	wrapper := filepath.Join(t.TempDir(), "git")
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, wrapper, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+gitcred.ShellQuote(w.argv)+"\n"+
		"printf 'GH_TOKEN=%s\\n' \"${GH_TOKEN:+set}\" >> "+gitcred.ShellQuote(w.argv)+"\nexec "+gitcred.ShellQuote(gitBin)+" \"$@\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	w.gitBin = wrapper
	w.env = append([]string{
		"PATH=" + filepath.Dir(wrapper) + string(filepath.ListSeparator) + os.Getenv("PATH"),
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_ALLOW_PROTOCOL=http", "GH_TOKEN=" + gitToken, gitcred.EnvSocket + "=" + creds.Path(),
		"GIT_AUTHOR_NAME=ccf-factory[bot]", "GIT_AUTHOR_EMAIL=bot@example.com", "GIT_COMMITTER_NAME=ccf-factory[bot]", "GIT_COMMITTER_EMAIL=bot@example.com",
	}, gitcred.ConfigEnv(gitcred.HelperConfig([]string{credHelper, "fx", "git-credential"}))...)

	// The checkout, cloned as the pipeline does (no user info in the URL).
	w.repo = filepath.Join(t.TempDir(), "repo")
	w.git(t.TempDir(), "clone", "--quiet", srv.URL+"/acme/app.git", w.repo)
	if creds.Answered() == 0 {
		t.Fatal("the clone didn't use the credential helper")
	}
	w.d = newDispatcher(t)
	w.run = newRun(t, w.d, func(c *pipeline.EffectorConfig) {
		c.Repo, c.RepoDir = "acme/app", w.repo
		if mut != nil {
			mut(c)
		}
	})
	return w
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// git runs git in dir with the run's environment (the credential helper).
func (w *workspace) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *workspace) commit(branch, file, body string) string {
	w.t.Helper()
	if branch != "" {
		w.git(w.repo, "checkout", "--quiet", "-B", branch)
	}
	writeFile(w.t, filepath.Join(w.repo, file), body)
	w.git(w.repo, "add", "-A")
	w.git(w.repo, "commit", "--quiet", "--no-gpg-sign", "-m", "change "+file)
	return w.git(w.repo, "rev-parse", "HEAD")
}

// effector is the run's Effector with the git wrapper and the run's environment.
func (w *workspace) effector(gh string) *effector.Effector {
	w.t.Helper()
	return effector.New(effector.Options{API: client(w.t, w.d.ts.URL), TaskID: taskID, Config: &w.run.cfg,
		State: filepath.Join(w.t.TempDir(), "state"), Git: w.gitBin, GH: gh, Env: w.env})
}

func (w *workspace) do(e *effector.Effector, op string, args map[string]any) (map[string]any, error) {
	w.t.Helper()
	return e.Do(context.Background(), op, args)
}

// remoteSHA is a branch's commit in the bare repo ("" when it doesn't exist).
func (w *workspace) remoteSHA(branch string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = w.bare
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// checkNoToken: no git command line held the token, git never got GH_TOKEN, and the token is
// in no file of the checkout's .git.
func (w *workspace) checkNoToken() {
	w.t.Helper()
	f, err := os.Open(w.argv)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
		if strings.Contains(sc.Text(), gitToken) {
			w.t.Errorf("a git command line holds the token: %q", sc.Text())
		}
		if sc.Text() == "GH_TOKEN=set" {
			w.t.Error("git got GH_TOKEN")
		}
	}
	if lines == 0 {
		w.t.Error("the git wrapper logged nothing")
	}
	_ = filepath.Walk(filepath.Join(w.repo, ".git"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), gitToken) {
			w.t.Errorf("the token is in %s", p)
		}
		return nil
	})
}

func (w *workspace) lastRecord() map[string]any {
	w.t.Helper()
	reqs := w.d.requests()
	if len(reqs) == 0 {
		w.t.Fatal("nothing recorded")
	}
	return reqs[len(reqs)-1].Body
}

// TestPush: factory push pushes through the credential helper and records push-branch.
func TestPush(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t, nil)
	e := w.effector("")
	sha := w.commit("factory/tsk_1", "docs/intro.md", "# Intro\n")
	out, err := w.do(e, "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.remoteSHA("factory/tsk_1"); got != sha {
		t.Fatalf("remote factory/tsk_1 = %q, want %q", got, sha)
	}
	rec := w.lastRecord()
	args := rec["args"].(map[string]any)
	if rec["op"] != "push-branch" || rec["target"] != nil || args["branch"] != "factory/tsk_1" || args["ref"] != "refs/heads/factory/tsk_1" || args["sha"] != sha {
		t.Errorf("record %v", rec)
	}
	if out["sha"] != sha || out["effectId"] == "" {
		t.Errorf("output %v", out)
	}
	// A rewritten branch needs --force-with-lease.
	w.git(w.repo, "commit", "--quiet", "--amend", "--no-gpg-sign", "-m", "amended")
	if _, err := w.do(e, "push", map[string]any{"branch": "factory/tsk_1"}); err == nil {
		t.Fatal("a non-fast-forward push without --force-with-lease succeeded")
	} else if code := exitOf(err); code != effector.ExitGit {
		t.Errorf("exit %d", code)
	}
	amended := w.git(w.repo, "rev-parse", "HEAD")
	if _, err := w.do(e, "push", map[string]any{"forceWithLease": true}); err != nil {
		t.Fatal(err)
	}
	if w.remoteSHA("factory/tsk_1") != amended || w.lastRecord()["args"].(map[string]any)["forced"] != true {
		t.Errorf("forced push: %s %v", w.remoteSHA("factory/tsk_1"), w.lastRecord())
	}
	// Refusals: a bad branch name, a missing branch, a detached HEAD.
	for _, tt := range []struct {
		args map[string]any
		msg  string
	}{
		{map[string]any{"branch": "--upload-pack=x"}, "is not a branch name"},
		{map[string]any{"branch": "nope"}, `there is no local branch "nope"`},
	} {
		if _, err := w.do(e, "push", tt.args); err == nil || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%v: %v", tt.args, err)
		}
	}
	w.git(w.repo, "checkout", "--quiet", "--detach")
	if _, err := w.do(e, "push", nil); err == nil || !strings.Contains(err.Error(), "HEAD is detached") {
		t.Errorf("detached: %v", err)
	}
	w.checkNoToken()
}

// TestPushNotAllowed: the local pre-check refuses before git runs.
func TestPushNotAllowed(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t, func(c *pipeline.EffectorConfig) { c.Allow = []string{"comment"} })
	w.commit("factory/tsk_1", "a.md", "a\n")
	_, err := w.do(w.effector(""), "push", nil)
	if exitOf(err) != effector.ExitNotAllowed {
		t.Fatalf("push: %v", err)
	}
	if w.remoteSHA("factory/tsk_1") != "" || len(w.d.requests()) != 0 {
		t.Error("a refused push reached the remote or the dispatcher")
	}
}

// TestStackGit: the git fallback of stack push and stack rebase.
func TestStackGit(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t, nil)
	e := w.effector(filepath.Join(t.TempDir(), "no-gh"))
	s1 := w.commit("stack/1", "one.md", "one\n")
	s2 := w.commit("stack/2", "two.md", "two\n")
	w.git(w.repo, "branch", "unrelated", "origin/main")
	out, err := w.do(e, "stack_push", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.remoteSHA("stack/1") != s1 || w.remoteSHA("stack/2") != s2 || w.remoteSHA("unrelated") != "" {
		t.Fatalf("pushed: %s %s %s", w.remoteSHA("stack/1"), w.remoteSHA("stack/2"), w.remoteSHA("unrelated"))
	}
	rec := w.lastRecord()
	args := rec["args"].(map[string]any)
	if rec["op"] != "stack.push" || args["via"] != "git" || args["base"] != "main" ||
		!slices.Equal(toStrings(args["branches"]), []string{"stack/1", "stack/2"}) || args["shas"].(map[string]any)["stack/2"] != s2 {
		t.Errorf("record %v", rec)
	}
	if out["via"] != "git" {
		t.Errorf("output %v", out)
	}

	// main moves on origin; the stack is rebased onto it, both branches updated.
	other := filepath.Join(t.TempDir(), "other")
	w.git(t.TempDir(), "clone", "--quiet", w.server.URL+"/acme/app.git", other)
	writeFile(t, filepath.Join(other, "main.md"), "main\n")
	w.git(other, "add", "-A")
	w.git(other, "commit", "--quiet", "--no-gpg-sign", "-m", "main moves")
	w.git(other, "push", "--quiet", "origin", "main")
	newMain := w.git(other, "rev-parse", "HEAD")
	if _, err := w.do(e, "stack_rebase", nil); err != nil {
		t.Fatal(err)
	}
	rec = w.lastRecord()
	args = rec["args"].(map[string]any)
	if rec["op"] != "stack.rebase" || args["onto"] != newMain || args["head"] != "stack/2" || args["via"] != "git" {
		t.Errorf("record %v", rec)
	}
	for _, b := range []string{"stack/1", "stack/2"} {
		if base := w.git(w.repo, "merge-base", newMain, b); base != newMain {
			t.Errorf("%s isn't on the new main", b)
		}
	}
	if w.git(w.repo, "merge-base", "stack/1", "stack/2") != w.git(w.repo, "rev-parse", "stack/1") {
		t.Error("stack/2 isn't on stack/1 any more")
	}
	// The rewritten stack is pushed with a lease.
	if _, err := w.do(e, "stack_push", map[string]any{"branches": []any{"stack/1", "stack/2"}}); err != nil {
		t.Fatal(err)
	}
	if w.remoteSHA("stack/2") != w.git(w.repo, "rev-parse", "stack/2") {
		t.Error("the rebased stack wasn't pushed")
	}

	// A conflict stops the rebase and says how to go on.
	writeFile(t, filepath.Join(other, "two.md"), "conflicting\n")
	w.git(other, "add", "-A")
	w.git(other, "commit", "--quiet", "--no-gpg-sign", "-m", "conflict")
	w.git(other, "push", "--quiet", "origin", "main")
	_, err = w.do(e, "stack_rebase", nil)
	if exitOf(err) != effector.ExitGit || !strings.Contains(err.Error(), "stopped on conflicts in: two.md") ||
		!strings.Contains(err.Error(), "git rebase --continue") {
		t.Errorf("conflict: %v", err)
	}
	w.git(w.repo, "rebase", "--abort")
	w.checkNoToken()
}

// exitOf is err's exit code (-1 when it isn't an *effector.Error, 0 for nil).
func exitOf(err error) int {
	if err == nil {
		return 0
	}
	var fe *effector.Error
	if errors.As(err, &fe) {
		return fe.Exit
	}
	return -1
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// TestStackGH: with gh-stack's state in the checkout and `gh stack` available, the ops
// run gh stack with the run's scoped GH_TOKEN (never an argument) and record the op.
func TestStackGH(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t, nil)
	writeFile(t, filepath.Join(w.repo, ".git", "gh-stack"), "{}\n")
	log := filepath.Join(t.TempDir(), "gh.log")
	gh := filepath.Join(t.TempDir(), "gh")
	// gh stack push runs git itself, as gh-stack does: with gh's environment (its GH_TOKEN
	// dropped, as the run's git never has it), so through the credential helper and
	// gitsafe's pairs, in a checkout with planted hooks.
	writeFile(t, gh, "#!/bin/sh\nprintf '%s|token=%s|prompt=%s\\n' \"$*\" \"${GH_TOKEN:+set}\" \"$GH_PROMPT_DISABLED\" >> "+gitcred.ShellQuote(log)+"\n"+
		"case \"$*\" in 'stack push --remote origin') GH_TOKEN= exec git push --quiet origin refs/heads/stack/1:refs/heads/stack/1;; esac\n"+
		"case \"$*\" in 'stack --help'|'stack rebase --remote origin') exit 0;; esac\necho unknown >&2\nexit 1\n")
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	e := w.effector(gh)
	sha := w.commit("stack/1", "one.md", "one\n")
	trap := gitsafetest.Plant(t, w.repo, gitsafetest.All)
	if _, err := w.do(e, "stack_push", nil); err != nil {
		t.Fatal(err)
	}
	if got := w.remoteSHA("stack/1"); got != sha {
		t.Errorf("gh stack's git push: remote stack/1 = %q, want %q", got, sha)
	}
	if _, err := w.do(e, "stack_rebase", nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	want := "stack --help|token=set|prompt=1\nstack push --remote origin|token=set|prompt=1\nstack --help|token=set|prompt=1\nstack rebase --remote origin|token=set|prompt=1\n"
	if string(b) != want {
		t.Errorf("gh calls:\n%s\nwant\n%s", b, want)
	}
	if strings.Contains(string(b), gitToken) {
		t.Error("gh got the token as an argument")
	}
	rec := w.lastRecord()
	if rec["op"] != "stack.rebase" || rec["args"].(map[string]any)["via"] != "gh-stack" {
		t.Errorf("record %v", rec)
	}
	// --git forces the fallback; --branches is the fallback's only.
	if _, err := w.do(e, "stack_push", map[string]any{"branches": []any{"stack/1"}}); err == nil || !strings.Contains(err.Error(), "--branches is for the git fallback") {
		t.Errorf("--branches with gh stack: %v", err)
	}
	if _, err := w.do(e, "stack_push", map[string]any{"git": true}); err != nil {
		t.Fatal(err)
	}
	if w.lastRecord()["args"].(map[string]any)["via"] != "git" {
		t.Error("--git didn't use git")
	}
	if fired := trap.Fired(t); len(fired) > 0 {
		t.Errorf("planted code ran: %v", fired)
	}
	w.checkNoToken()
}

// TestGitIgnoresPlantedCode: the git-side ops never run the hooks or the fsmonitor an agent
// can plant in the checkout's .git (the Effector may run outside the harness's sandbox).
func TestGitIgnoresPlantedCode(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t, nil)
	e := w.effector(filepath.Join(t.TempDir(), "no-gh"))
	w.commit("stack/1", "one.md", "one\n")
	w.commit("stack/2", "two.md", "two\n")
	// main moves on origin, with a change that conflicts with stack/2.
	other := filepath.Join(t.TempDir(), "other")
	w.git(t.TempDir(), "clone", "--quiet", w.server.URL+"/acme/app.git", other)
	writeFile(t, filepath.Join(other, "two.md"), "conflicting\n")
	w.git(other, "add", "-A")
	w.git(other, "commit", "--quiet", "--no-gpg-sign", "-m", "conflict")
	w.git(other, "push", "--quiet", "origin", "main")

	trap := gitsafetest.Plant(t, w.repo, gitsafetest.All)
	if _, err := w.do(e, "push", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.do(e, "stack_push", nil); err != nil {
		t.Fatal(err)
	}
	// The rebase checks out, rewrites and stops on the conflict (then lists it).
	if _, err := w.do(e, "stack_rebase", nil); exitOf(err) != effector.ExitGit || !strings.Contains(err.Error(), "two.md") {
		t.Fatalf("stack rebase: %v", err)
	}
	if fired := trap.Fired(t); len(fired) > 0 {
		t.Errorf("planted code ran: %v", fired)
	}
	// The fixture is live: the test's own git runs it.
	w.git(w.repo, "rebase", "--abort")
	if len(trap.Fired(t)) == 0 {
		t.Error("plain git ran nothing planted: the fixture is dead")
	}
	w.checkNoToken()
}
