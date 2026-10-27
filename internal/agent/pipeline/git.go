package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// defaultUsername goes with an installation token when the credential names none.
const defaultUsername = "x-access-token"

// repoRE is an owner/name repo; refRE a branch, tag or commit that can't be read as an
// option or a range.
var (
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./+-]*$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

func validRepo(r string) bool {
	return repoRE.MatchString(r) && !strings.Contains(r, "..")
}

func validRef(r string) bool {
	return r == "" || (refRE.MatchString(r) && !strings.Contains(r, "..") && !strings.HasSuffix(r, ".lock"))
}

// cloneBase is where the task's repos are cloned from.
func (r *run) cloneBase() (*url.URL, error) {
	raw := r.p.o.GitBaseURL
	if raw == "" {
		host := "github.com"
		if r.job.Git != nil {
			if h := r.job.Git.Get().Host; h != "" {
				host = h
			}
		}
		raw = "https://" + host
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("git base URL %q: want an http(s) URL without credentials", raw)
	}
	return u, nil
}

// credentialEntries are the repos the credential helper answers for, from the job's current
// credentials (read at every request, so a heartbeat's refresh applies at once): the task
// repo with the task token, and each private read repo with its own read-only token (the
// lease's readCredentials; a read-only token may also list further readRepos). Public read
// repos (publicReads) have no credential and no entry: they are cloned anonymously.
func credentialEntries(base *url.URL, creds *runner.Credentials) gitcred.Entries {
	return func() []gitcred.Entry {
		if creds == nil {
			return nil
		}
		var out []gitcred.Entry
		add := func(c apiclient.GitCredential, readOnly bool) {
			if c.Token == "" {
				return
			}
			user := c.Username
			if user == "" {
				user = defaultUsername
			}
			repos := []string{c.Repo}
			if readOnly {
				repos = append(repos, c.ReadRepos...)
			}
			for _, repo := range repos {
				if repo != "" {
					out = append(out, gitcred.Entry{Protocol: base.Scheme, Host: base.Host, Repo: repo, Username: user, Password: c.Token})
				}
			}
		}
		// The task token first: it wins for its repo.
		add(creds.Get(), false)
		for _, c := range creds.Reads() {
			add(c, true)
		}
		return out
	}
}

// startCredentials starts the in-memory credential helper and the git runner that uses it
// (run-pipeline.md §Steps 2). With GitAuthGateway there is no helper to start
// (workspace-run.md §Gateway mode): only the git runner, whose git goes through the gateway.
func (r *run) startCredentials(context.Context) error {
	base, err := r.cloneBase()
	if err != nil {
		return err
	}
	r.base = base
	if r.p.o.GitAuth != GitAuthGateway {
		srv, err := gitcred.Listen(r.p.o.SocketDir, credentialEntries(base, r.job.Git))
		if err != nil {
			return err
		}
		r.creds = srv
	}
	protos, err := r.allowedProtocols()
	if err != nil {
		return err
	}
	r.protocols = protos
	r.git = &gitRunner{bin: r.p.o.Git, env: r.gitEnv(), red: r.red}
	return nil
}

// allowedProtocols are the git transports runs may use (GIT_ALLOW_PROTOCOL): https, and
// http only when the clone base or a skills source is plain http (a loopback test server).
func (r *run) allowedProtocols() (string, error) {
	set := map[string]bool{"https": true, r.base.Scheme: true}
	for _, s := range r.task.Spec.Skills {
		src := r.skillsURL(s.Source)
		u, err := url.Parse(src)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return "", fmt.Errorf("skills source %q: want an http(s) URL without credentials", s.Source)
		}
		set[u.Scheme] = true
	}
	if set["http"] {
		return "https:http", nil
	}
	return "https", nil
}

// gitRunner runs git with the run's environment.
type gitRunner struct {
	bin string
	env []string
	red *redactor
}

// run runs git in dir and returns its trimmed stdout. Errors carry git's stderr, redacted.
func (g *gitRunner) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Dir = dir
	cmd.Env = g.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", firstArg(args), context.Cause(ctx))
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return "", fmt.Errorf("git %s: %w: %s", firstArg(args), err, g.red.String(msg))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && !strings.HasPrefix(a, "/") {
			return a
		}
	}
	return ""
}

// mountPlan is one repo to check out.
type mountPlan struct {
	harness.Mount
	// pr > 0 checks out the pull request's head (refs/pull/<pr>/head).
	pr int
	// primary is the checkout at repo/.
	primary bool
}

// planMounts works out the checkouts from the task (run-pipeline.md §Steps 1 and 3). The
// TaskSpec's workspace reaches the Task as context items of kind repo ("owner/name@ref");
// the primary checkout (repo/) is the credential's repo, writable when the credential may
// write contents (a rw mount, or a checkout other than read-only); the others go read-only
// under ro/<name>. Without repo items (workspace.checkout only) the primary is the task
// repo, at the pull request's head when the task is about one.
func planMounts(t v1alpha1.Task, cred apiclient.GitCredential, root string) ([]mountPlan, error) {
	writable := cred.Permissions["contents"] == "write"
	var items []mountPlan
	for _, c := range t.Spec.Context {
		if c.Kind != "repo" {
			continue
		}
		repo, ref, _ := strings.Cut(c.Ref, "@")
		items = append(items, mountPlan{Mount: harness.Mount{Repo: repo, Ref: ref}})
	}
	if len(items) == 0 {
		if cred.Repo == "" {
			return nil, nil
		}
		m := mountPlan{Mount: harness.Mount{Repo: cred.Repo, Dir: filepath.Join(root, "repo"), Writable: writable}, primary: true}
		m.pr = prNumber(t, cred.Repo)
		items = append(items, m)
	} else {
		primary := -1
		for i, m := range items {
			if strings.EqualFold(m.Repo, cred.Repo) {
				primary = i
				break
			}
		}
		taken := map[string]bool{}
		for i := range items {
			m := &items[i]
			if i == primary {
				m.Dir, m.Writable, m.primary = filepath.Join(root, "repo"), writable, true
				continue
			}
			owner, name, _ := strings.Cut(m.Repo, "/")
			dir := name
			if taken[strings.ToLower(dir)] {
				dir = owner + "-" + name
			}
			for n := 2; taken[strings.ToLower(dir)]; n++ {
				dir = owner + "-" + name + "-" + strconv.Itoa(n)
			}
			taken[strings.ToLower(dir)] = true
			m.Dir = filepath.Join(root, "ro", dir)
		}
		if primary > 0 {
			p := items[primary]
			copy(items[1:primary+1], items[:primary])
			items[0] = p
		}
	}
	for _, m := range items {
		if !validRepo(m.Repo) {
			return nil, fmt.Errorf("repo %q: want owner/name", m.Repo)
		}
		if !validRef(m.Ref) {
			return nil, fmt.Errorf("repo %s: ref %q is not a branch, tag or commit", m.Repo, m.Ref)
		}
	}
	return items, nil
}

// prNumber is the pull request the task targets in repo, or 0: the target ref
// "owner/name#n" when the task's context has it as a pr item.
func prNumber(t v1alpha1.Task, repo string) int {
	if t.Spec.Target == nil {
		return 0
	}
	ref := t.Spec.Target.Ref
	r, num, ok := strings.Cut(ref, "#")
	if !ok || !strings.EqualFold(r, repo) {
		return 0
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0
	}
	for _, c := range t.Spec.Context {
		if c.Kind == "pr" && c.Ref == ref {
			return n
		}
	}
	return 0
}

// cloneRepos checks out the task's repos (run-pipeline.md §Steps 3).
func (r *run) cloneRepos(ctx context.Context) error {
	var cred apiclient.GitCredential
	if r.job.Git != nil {
		cred = r.job.Git.Get()
	}
	plans, err := planMounts(r.task, cred, r.ws.Root)
	if err != nil {
		return err
	}
	for _, m := range plans {
		if err := r.clone(ctx, m); err != nil {
			return fmt.Errorf("%s: %w", m.Repo, err)
		}
		r.ws.Mounts = append(r.ws.Mounts, m.Mount)
		if m.primary {
			r.ws.Repo, r.ws.RepoWritable = m.Dir, m.Writable
		}
	}
	switch {
	case r.ws.Repo != "":
		r.ws.WorkDir = r.ws.Repo
	case len(r.ws.Mounts) > 0:
		r.ws.WorkDir = r.ws.Mounts[0].Dir
	}
	return nil
}

// clone checks out one repo: a partial clone (blobs fetched on demand, through the same
// credential helper) at the ref.
func (r *run) clone(ctx context.Context, m mountPlan) error {
	url := r.base.String() + "/" + m.Repo + ".git"
	g := r.git
	switch {
	case m.Ref == "":
		if _, err := g.run(ctx, r.ws.Root, "clone", "--quiet", "--filter=blob:none", "--", url, m.Dir); err != nil {
			return err
		}
	case commitRE.MatchString(m.Ref):
		if _, err := g.run(ctx, r.ws.Root, "clone", "--quiet", "--filter=blob:none", "--no-checkout", "--", url, m.Dir); err != nil {
			return err
		}
		if _, err := g.run(ctx, m.Dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", m.Ref); err != nil {
			return err
		}
	default:
		if _, err := g.run(ctx, r.ws.Root, "clone", "--quiet", "--filter=blob:none", "--branch", m.Ref, "--", url, m.Dir); err != nil {
			return err
		}
	}
	if m.pr > 0 {
		local := "refs/remotes/origin/pr/" + strconv.Itoa(m.pr)
		if _, err := g.run(ctx, m.Dir, "fetch", "--quiet", "origin", "+refs/pull/"+strconv.Itoa(m.pr)+"/head:"+local); err != nil {
			return err
		}
		if _, err := g.run(ctx, m.Dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", local); err != nil {
			return err
		}
	}
	if m.Writable {
		for _, kv := range [][2]string{{"user.name", r.p.o.BotName}, {"user.email", r.p.o.BotEmail}} {
			if _, err := g.run(ctx, m.Dir, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	}
	// A read-only checkout can't be pushed by mistake: its push URL uses a transport that
	// GIT_ALLOW_PROTOCOL refuses (the token's scope refuses it anyway).
	if _, err := g.run(ctx, m.Dir, "remote", "set-url", "--push", "origin", "read-only://this-checkout-is-read-only"); err != nil {
		return err
	}
	return nil
}
