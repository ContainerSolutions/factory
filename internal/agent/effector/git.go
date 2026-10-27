package effector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// remote is the workspace checkout's remote: the pipeline clones the task repo as origin.
const remote = "origin"

// maxOutput bounds the git or gh output kept for an error message.
const maxOutput = 4 << 10

// Variables git never needs: the gateway injects git's credential.
var gitDropped = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// gitError is a failed git or gh command (exit 5) with the tail of its output.
func gitError(what string, err error, out []byte) *Error {
	tail := strings.TrimSpace(string(out))
	if len(tail) > maxOutput {
		tail = "…" + tail[len(tail)-maxOutput:]
	}
	msg := fmt.Sprintf("%s failed: %v", what, err)
	if tail != "" {
		msg += "\n" + tail
	}
	return &Error{Exit: ExitGit, Msg: msg, Err: err, Extra: map[string]any{"output": tail}}
}

// env is the base environment of git and gh: Options.Env, else this process's.
func (e *Effector) env() []string {
	if e.o.Env != nil {
		return e.o.Env
	}
	return os.Environ()
}

// gitEnv is git's environment: the run's, which holds the credential helper's process-scoped
// config, without any token variable, and never prompting.
func (e *Effector) gitEnv() []string {
	var out []string
	for _, kv := range e.env() {
		k, _, _ := strings.Cut(kv, "=")
		drop := k == "GIT_TERMINAL_PROMPT"
		for _, d := range gitDropped {
			drop = drop || k == d
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// git runs git in the workspace and returns its trimmed stdout. No argument ever carries a
// credential.
func (e *Effector) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.o.Git, args...)
	cmd.Dir = e.o.Dir
	cmd.Env = e.gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", gitError("git "+args[0], err, append(stderr.Bytes(), stdout.Bytes()...))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// branchName checks a branch name: what git accepts, and never option-like.
func (e *Effector) branchName(ctx context.Context, b string) error {
	if b == "" || strings.HasPrefix(b, "-") || strings.ContainsAny(b, " \t\n:") {
		return usagef("%q is not a branch name", b)
	}
	if _, err := e.git(ctx, "check-ref-format", "--branch", b); err != nil {
		return usagef("%q is not a branch name", b)
	}
	return nil
}

// sha is the commit a local branch points at.
func (e *Effector) sha(ctx context.Context, branch string) (string, error) {
	s, err := e.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", usagef("there is no local branch %q", branch)
	}
	return s, nil
}

func (e *Effector) currentBranch(ctx context.Context) (string, error) {
	b, err := e.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		return "", usagef("HEAD is detached: check a branch out or pass --branch")
	}
	return b, nil
}

func runPush(ctx context.Context, e *Effector, a args) (map[string]any, error) {
	e.gitMu.Lock()
	defer e.gitMu.Unlock()
	if err := e.precheck(v1alpha1.OpPushBranch, nil); err != nil {
		return nil, err
	}
	branch := a.str("branch")
	if branch == "" {
		var err error
		if branch, err = e.currentBranch(ctx); err != nil {
			return nil, err
		}
	}
	if err := e.branchName(ctx, branch); err != nil {
		return nil, err
	}
	sha, err := e.sha(ctx, branch)
	if err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	push := []string{"push", "--porcelain"}
	if a.flag("forceWithLease") {
		push = append(push, "--force-with-lease="+ref)
	}
	if _, err := e.git(ctx, append(push, remote, ref+":"+ref)...); err != nil {
		return nil, err
	}
	rec := map[string]any{"branch": branch, "ref": ref, "sha": sha}
	if a.flag("forceWithLease") {
		rec["forced"] = true
	}
	return e.record(ctx, v1alpha1.OpPushBranch, rec, a.str("key"))
}

// record records a git-side op that already ran (effector.md §Behaviour 4).
func (e *Effector) record(ctx context.Context, op v1alpha1.EffectOp, rec map[string]any, key string) (map[string]any, error) {
	out, err := e.apply(ctx, op, "", rec, key)
	if err != nil {
		var fe *Error
		if errors.As(err, &fe) {
			fe.Msg = fmt.Sprintf("the %s ran in the workspace, but recording it failed: %s", op, fe.Msg)
		}
		return nil, err
	}
	for k, v := range rec {
		out[k] = v
	}
	return out, nil
}

// base is the stack's trunk: --base, else origin's default branch, else main.
func (e *Effector) base(ctx context.Context, a args) (string, error) {
	if b := a.str("base"); b != "" {
		return b, e.branchName(ctx, b)
	}
	if h, err := e.git(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(h, remote+"/"); ok && b != "" {
			return b, nil
		}
	}
	return "main", nil
}

// stackBranches are the stack's branches, bottom first: the given ones, else the local
// branches merged into HEAD but not into origin/<base>.
func (e *Effector) stackBranches(ctx context.Context, base string, given []string) ([]string, error) {
	trunk := "refs/remotes/" + remote + "/" + base
	if len(given) > 0 {
		for _, b := range given {
			if err := e.branchName(ctx, b); err != nil {
				return nil, err
			}
			if _, err := e.sha(ctx, b); err != nil {
				return nil, err
			}
		}
		return given, nil
	}
	if _, err := e.git(ctx, "rev-parse", "--verify", "--quiet", trunk); err != nil {
		return nil, usagef("%s/%s doesn't exist here: pass --base or --branches", remote, base)
	}
	list, err := e.git(ctx, "for-each-ref", "--format=%(refname:short)", "--merged=HEAD", "--no-merged="+trunk, "refs/heads/")
	if err != nil {
		return nil, err
	}
	type branch struct {
		name  string
		ahead int
	}
	var bs []branch
	for _, b := range strings.Fields(list) {
		if b == base {
			continue
		}
		n, err := e.git(ctx, "rev-list", "--count", trunk+"..refs/heads/"+b)
		if err != nil {
			return nil, err
		}
		ahead, _ := strconv.Atoi(n)
		bs = append(bs, branch{b, ahead})
	}
	if len(bs) == 0 {
		return nil, usagef("no stack branches between %s/%s and HEAD", remote, base)
	}
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].ahead < bs[j].ahead })
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.name
	}
	return out, nil
}

func (e *Effector) shas(ctx context.Context, branches []string) (map[string]any, error) {
	out := map[string]any{}
	for _, b := range branches {
		s, err := e.sha(ctx, b)
		if err != nil {
			return nil, err
		}
		out[b] = s
	}
	return out, nil
}

func runStackPush(ctx context.Context, e *Effector, a args) (map[string]any, error) {
	e.gitMu.Lock()
	defer e.gitMu.Unlock()
	if err := e.precheck(v1alpha1.OpStackPush, nil); err != nil {
		return nil, err
	}
	base, err := e.base(ctx, a)
	if err != nil {
		return nil, err
	}
	// --git is accepted and ignored: the push is always git's (gh stack writes through the
	// GitHub API, which the gateway refuses; workspace-run.md §The Effector in the workspace
	// runtime).
	bs, err := e.stackBranches(ctx, base, a.list("branches"))
	if err != nil {
		return nil, err
	}
	push := []string{"push", "--porcelain", "--atomic"}
	for _, b := range bs {
		push = append(push, "--force-with-lease=refs/heads/"+b)
	}
	push = append(push, remote)
	for _, b := range bs {
		push = append(push, "refs/heads/"+b+":refs/heads/"+b)
	}
	if _, err := e.git(ctx, push...); err != nil {
		return nil, err
	}
	shas, err := e.shas(ctx, bs)
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"base": base, "via": "git", "branches": bs, "shas": shas}
	return e.record(ctx, v1alpha1.OpStackPush, rec, a.str("key"))
}

func runStackRebase(ctx context.Context, e *Effector, a args) (map[string]any, error) {
	e.gitMu.Lock()
	defer e.gitMu.Unlock()
	if err := e.precheck(v1alpha1.OpStackRebase, nil); err != nil {
		return nil, err
	}
	base, err := e.base(ctx, a)
	if err != nil {
		return nil, err
	}
	// --git is accepted and ignored: the rebase is always git's (as stack push).
	head, err := e.currentBranch(ctx)
	if err != nil {
		return nil, err
	}
	trunk := "refs/remotes/" + remote + "/" + base
	if _, err := e.git(ctx, "fetch", "--quiet", remote, "+refs/heads/"+base+":"+trunk); err != nil {
		return nil, err
	}
	bs, err := e.stackBranches(ctx, base, nil)
	if err != nil {
		return nil, err
	}
	if _, err := e.git(ctx, "rebase", "--update-refs", trunk); err != nil {
		if conflicts := e.conflicts(ctx); conflicts != "" {
			var fe *Error
			if errors.As(err, &fe) {
				fe.Msg = fmt.Sprintf("the rebase of %s onto %s/%s stopped on conflicts in: %s. Resolve them, run `git rebase --continue`, then `factory stack push`", head, remote, base, conflicts)
			}
		}
		return nil, err
	}
	onto, err := e.git(ctx, "rev-parse", "--verify", trunk)
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"base": base, "via": "git", "onto": onto, "head": head, "branches": bs}
	if b, err := e.currentBranch(ctx); err == nil {
		if s, err := e.sha(ctx, b); err == nil {
			rec["sha"] = s
		}
	}
	return e.record(ctx, v1alpha1.OpStackRebase, rec, a.str("key"))
}

// conflicts lists the unmerged paths of a stopped rebase ("" when there are none).
func (e *Effector) conflicts(ctx context.Context) string {
	out, err := e.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out), ", ")
}
