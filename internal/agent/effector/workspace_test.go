package effector_test

import (
	"bytes"
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
	"github.com/compliance-framework/factory/internal/agent/internal/fakegateway"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
)

// The repos' tokens, which only the gateway holds.
const (
	appToken = "ghs_appRepoTokenOnlyTheGatewayHolds01"
	roToken  = "ghs_roRepoTokenOnlyTheGatewayHolds012"
)

// vmWorkspace is a VM workspace's view of the world (workspace-run.md §The Effector in the
// workspace runtime): the private repos acme/app (rw) and acme/ro (read) on github.com and the
// dispatcher at runbundle.DefaultDispatcherURL, all reached only through the fakegateway,
// which holds every credential. Its environment is what vmrun sets: the proxies, the CA, a
// placeholder GH_TOKEN, FACTORY_RUNTIME=workspace, and no FACTORY_TOKEN or credential helper.
type vmWorkspace struct {
	t     *testing.T
	gw    *fakegateway.Gateway
	d     *dispatcher
	root  string   // the bare repos
	env   []string // git's and the Effector's base environment
	ghLog string   // the fake gh's log: it must stay empty
}

func newVMWorkspace(t *testing.T) *vmWorkspace {
	t.Helper()
	backend := filepath.Join(testGit(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	root := t.TempDir()
	for _, repo := range []string{"acme/app", "acme/ro"} {
		seed := t.TempDir()
		testGit(t, seed, "init", "--quiet", "-b", "main")
		writeFile(t, filepath.Join(seed, "README.md"), "# "+repo+"\n")
		testGit(t, seed, "add", "-A")
		testGit(t, seed, "commit", "--quiet", "--no-gpg-sign", "-m", "initial")
		bare := filepath.Join(root, repo+".git")
		testGit(t, seed, "clone", "--quiet", "--bare", seed, bare)
		testGit(t, bare, "config", "http.receivepack", "true")
	}
	// github.com's git: each repo answers only its own token.
	tokens := map[string]string{"/acme/app.git/": appToken, "/acme/ro.git/": roToken}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, p, ok := r.BasicAuth()
		for prefix, tok := range tokens {
			if strings.HasPrefix(r.URL.Path, prefix) && ok && p == tok {
				h.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		http.Error(w, "auth required", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)

	d := newDispatcher(t)
	gw := fakegateway.Start(t, fakegateway.Config{RunToken: fakegateway.RunToken, Dispatcher: d.ts.URL, TaskID: taskID,
		GitUpstream: upstream.URL, Repos: map[string]fakegateway.Repo{"acme/app": {Token: appToken, RW: true}, "acme/ro": {Token: roToken}}})
	home, bin := t.TempDir(), t.TempDir()
	ca := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(ca, gw.CA().CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	// A gh that would run gh stack: logged, so the test sees it's never called.
	w := &vmWorkspace{t: t, gw: gw, d: d, root: root, ghLog: filepath.Join(t.TempDir(), "gh.log")}
	writeFile(t, filepath.Join(bin, "gh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+gitcred.ShellQuote(w.ghLog)+"\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.env = []string{
		"PATH=" + bin + string(filepath.ListSeparator) + os.Getenv("PATH"), "HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"), "GIT_TERMINAL_PROMPT=0",
		"GIT_SSL_CAINFO=" + ca, "SSL_CERT_FILE=" + ca,
		"HTTPS_PROXY=" + gw.URL(), "https_proxy=" + gw.URL(), "HTTP_PROXY=" + gw.URL(), "http_proxy=" + gw.URL(), "NO_PROXY=", "no_proxy=",
		pipeline.EnvGHToken + "=" + fakegateway.DefaultPlaceholder,
		"GIT_AUTHOR_NAME=ccf-factory[bot]", "GIT_AUTHOR_EMAIL=bot@example.com", "GIT_COMMITTER_NAME=ccf-factory[bot]", "GIT_COMMITTER_EMAIL=bot@example.com",
	}
	return w
}

// git runs git in dir with the workspace's environment.
func (w *vmWorkspace) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// checkout clones repo from github.com through the gateway, as the pipeline's gateway mode
// does, and returns it with the run (its EffectorConfig) of a task on that repo.
func (w *vmWorkspace) checkout(repo string) (string, *run) {
	w.t.Helper()
	dir := filepath.Join(w.t.TempDir(), "repo")
	w.git(w.t.TempDir(), "clone", "--quiet", "https://github.com/"+repo+".git", dir)
	r := newRun(w.t, w.d, func(c *pipeline.EffectorConfig) { c.Repo, c.RepoDir = repo, dir })
	return dir, r
}

// fx runs `factory <argv>` as its own process (the test binary's TestMain), with the
// workspace's environment and the run's FACTORY_* variables but no FACTORY_TOKEN, as the VM's
// harness runs it.
func (w *vmWorkspace) fx(r *run, argv ...string) (int, string, string) {
	w.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		w.t.Fatal(err)
	}
	env := slices.Clone(w.env)
	for k, v := range r.env {
		if k != pipeline.EnvToken && k != pipeline.EnvAPI {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, pipeline.EnvAPI+"="+runbundle.DefaultDispatcherURL, pipeline.EnvRuntime+"="+pipeline.RuntimeWorkspace)
	cmd := exec.CommandContext(w.t.Context(), exe, append([]string{"fx"}, argv...)...)
	cmd.Dir, cmd.Env = r.cfg.RepoDir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			w.t.Fatalf("factory %v: %v", argv, err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func (w *vmWorkspace) remoteSHA(repo, branch string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = filepath.Join(w.root, repo+".git")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (w *vmWorkspace) commit(dir, file string) string {
	w.t.Helper()
	writeFile(w.t, filepath.Join(dir, file), file+"\n")
	w.git(dir, "add", "-A")
	w.git(dir, "commit", "--quiet", "--no-gpg-sign", "-m", "change "+file)
	return w.git(dir, "rev-parse", "HEAD")
}

// TestWorkspaceRuntime: in a VM workspace the Effector holds no credential. Its effects carry
// no Authorization and reach the dispatcher through HTTP_PROXY, where the gateway adds the run
// token; its pushes reach github.com through HTTPS_PROXY, where the gateway adds the repo's
// credential and refuses a read repo; and the stack ops take the git fallback even when gh
// stack could run.
func TestWorkspaceRuntime(t *testing.T) {
	t.Parallel()
	w := newVMWorkspace(t)
	app, r := w.checkout("acme/app")

	// factory push lands, and its record reaches the dispatcher with the run token.
	w.git(app, "checkout", "--quiet", "-b", "factory/tsk_1")
	sha := w.commit(app, "intro.md")
	if code, _, stderr := w.fx(r, "push"); code != effector.ExitOK {
		t.Fatalf("push: %d %s", code, stderr)
	}
	if got := w.remoteSHA("acme/app", "factory/tsk_1"); got != sha {
		t.Errorf("remote factory/tsk_1 = %q, want %q", got, sha)
	}

	// The stack: gh-stack's state is there and gh answers, but the git fallback runs.
	writeFile(t, filepath.Join(app, ".git", "gh-stack"), "{}\n")
	w.git(app, "checkout", "--quiet", "-b", "stack/1", "origin/main")
	s1 := w.commit(app, "one.md")
	w.git(app, "checkout", "--quiet", "-b", "stack/2")
	s2 := w.commit(app, "two.md")
	code, stdout, stderr := w.fx(r, "stack", "push")
	if code != effector.ExitOK || !strings.Contains(stdout, `"via":"git"`) {
		t.Fatalf("stack push: %d %s %s", code, stdout, stderr)
	}
	if w.remoteSHA("acme/app", "stack/1") != s1 || w.remoteSHA("acme/app", "stack/2") != s2 {
		t.Errorf("the stack wasn't pushed")
	}
	// main moves on github.com; the stack is rebased onto it and pushed again.
	other := filepath.Join(t.TempDir(), "other")
	w.git(t.TempDir(), "clone", "--quiet", "https://github.com/acme/app.git", other)
	newMain := w.commit(other, "main.md")
	w.git(other, "push", "--quiet", "origin", "main")
	if code, stdout, stderr := w.fx(r, "stack", "rebase"); code != effector.ExitOK || !strings.Contains(stdout, `"via":"git"`) || !strings.Contains(stdout, newMain) {
		t.Fatalf("stack rebase: %d %s %s", code, stdout, stderr)
	}
	if code, _, stderr := w.fx(r, "stack", "push"); code != effector.ExitOK {
		t.Fatalf("stack push after the rebase: %d %s", code, stderr)
	}
	if w.remoteSHA("acme/app", "stack/2") != w.git(app, "rev-parse", "stack/2") {
		t.Error("the rebased stack wasn't pushed")
	}
	if b, _ := os.ReadFile(w.ghLog); len(b) > 0 {
		t.Errorf("gh ran in a workspace:\n%s", b)
	}

	// An API op: no client credential, the run token added on the way.
	if code, _, stderr := w.fx(r, "comment", "--body", "Planning the epic."); code != effector.ExitOK {
		t.Fatalf("comment: %d %s", code, stderr)
	}

	// A push to a read repo is refused by the gateway: a git failure, and nothing recorded.
	ro, rr := w.checkout("acme/ro")
	w.git(ro, "checkout", "--quiet", "-b", "factory/tsk_1")
	w.commit(ro, "nope.md")
	before := len(w.d.requests())
	code, _, stderr = w.fx(rr, "push")
	if code != effector.ExitGit || !strings.Contains(stderr, "403") {
		t.Errorf("push to the read repo: %d %s", code, stderr)
	}
	if w.remoteSHA("acme/ro", "factory/tsk_1") != "" || len(w.d.requests()) != before {
		t.Error("a refused push reached the remote or the dispatcher")
	}

	reqs := w.d.requests()
	var ops []string
	for _, q := range reqs {
		ops = append(ops, q.Body["op"].(string))
		if q.Auth != "Bearer "+fakegateway.RunToken {
			t.Errorf("%s reached the dispatcher with Authorization %q, want the run token", q.Body["op"], q.Auth)
		}
	}
	if want := []string{"push-branch", "stack.push", "stack.rebase", "stack.push", "comment"}; !slices.Equal(ops, want) {
		t.Errorf("dispatcher ops %v, want %v", ops, want)
	}
	u, err := url.Parse(runbundle.DefaultDispatcherURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range w.gw.Records() {
		if rec.Host == u.Hostname() && rec.ClientAuth != fakegateway.ClientAuthNone {
			t.Errorf("an effect arrived with Authorization (%s)", rec.ClientAuth)
		}
	}
	w.gw.AssertNoClientCredential(t)
	for _, dir := range []string{app, ro} {
		_ = filepath.Walk(filepath.Join(dir, ".git"), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			for _, tok := range []string{appToken, roToken, fakegateway.RunToken} {
				if bytes.Contains(b, []byte(tok)) {
					t.Errorf("a credential is in %s", p)
				}
			}
			return nil
		})
	}
}
