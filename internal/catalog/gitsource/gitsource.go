// Package gitsource loads a catalog from a git ref: a shallow fetch into a temporary
// directory with the git CLI, then catalog.LoadDir on the catalog's directory in it, with the
// commit as the snapshot's SHA (design/workstreams/dispatcher-core/LLD/catalog.md §Loading).
// Head resolves a ref to its commit with git ls-remote, for catalog-sync's polling; HeadAuth
// does the same on a private repository with a token (Auth).
//
// Errors never carry credentials: user info in the repository URL (a token in
// https://x-access-token:<token>@host/…) and an Auth token are redacted from every error
// message.
//
// It is the one part of catalog loading that runs a process and touches the network, so it
// is kept out of the pure catalog package. It shells out to git rather than linking go-git,
// which isn't on the dependency allow-list.
package gitsource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ContainerSolutions/factory/internal/catalog"
)

// ErrGit is wrapped by every failure to fetch the ref (git missing, network, unknown ref).
var ErrGit = errors.New("git")

// allowedProtocols are the git transports a catalog may come from (GIT_ALLOW_PROTOCOL): no
// ext:: or other command-running transports.
const allowedProtocols = "https:ssh:git:file"

// Load fetches ref (a branch, tag or commit) of repoURL at depth 1 into a temporary
// directory, loads the catalog in its subdirectory dir ("catalog"; "" or "." for the repo
// root) and removes the checkout. The snapshot's SHA is the fetched commit. The []Error is
// LoadDir's; err is set only when the ref couldn't be fetched (wrapping ErrGit) or dir
// doesn't exist.
func Load(ctx context.Context, repoURL, ref, dir string, opts catalog.Options) (_ *catalog.Catalog, _ []catalog.Error, err error) {
	defer func() { err = redactError(err, repoURL) }()
	if repoURL == "" || ref == "" {
		return nil, nil, fmt.Errorf("%w: repository URL and ref are required", ErrGit)
	}
	tmp, err := os.MkdirTemp("", "factory-catalog-*")
	if err != nil {
		return nil, nil, fmt.Errorf("temporary checkout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	steps := [][]string{
		{"init", "--quiet", tmp},
		{"-C", tmp, "fetch", "--quiet", "--depth", "1", "--no-tags", "--", repoURL, ref},
		{"-C", tmp, "-c", "advice.detachedHead=false", "checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := git(ctx, nil, args...); err != nil {
			return nil, nil, err
		}
	}
	sha, err := git(ctx, nil, "-C", tmp, "rev-parse", "HEAD")
	if err != nil {
		return nil, nil, err
	}

	sub := path.Clean(strings.TrimPrefix(dir, "/"))
	if sub == ".." || strings.HasPrefix(sub, "../") {
		return nil, nil, fmt.Errorf("catalog directory %q is outside the repository", dir)
	}
	// os.Root confines every read to the checkout: a catalog directory (or anything under it)
	// that is a symlink pointing outside the repository can't be followed out of it.
	rt, err := os.OpenRoot(tmp)
	if err != nil {
		return nil, nil, fmt.Errorf("temporary checkout: %w", err)
	}
	defer func() { _ = rt.Close() }()
	root := rt.FS()
	if info, err := fs.Stat(root, sub); err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("catalog directory %q does not exist at %s", dir, ref)
	}
	fsys, err := fs.Sub(root, sub)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog directory %q: %w", dir, err)
	}
	opts.SHA = sha
	c, errs := catalog.LoadDir(fsys, opts)
	return c, errs, nil
}

// commitRE matches a full commit SHA (SHA-1 or SHA-256).
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// IsCommit reports whether ref is a full commit SHA, which Head returns as is (no git, no
// credential needed).
func IsCommit(ref string) bool { return commitRE.MatchString(ref) }

// Auth is an HTTP credential for a private repository: a token and the username that goes
// with it ("x-access-token" for a GitHub installation token, the default). The zero value is
// anonymous.
type Auth struct {
	Username string
	Token    string
}

// defaultUsername goes with a token when Auth names no username.
const defaultUsername = "x-access-token"

// header is the Authorization header value: Basic base64(username:token).
func (a Auth) header() string {
	user := a.Username
	if user == "" {
		user = defaultUsername
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+a.Token))
}

// config is the process-scoped git config that sends the credential: an extra Authorization
// header scoped to repoURL (http.<url>.extraHeader applies to that URL and the paths under
// it; curl doesn't forward it to another host on a redirect), as actions/checkout does. It
// reaches git in its environment (GIT_CONFIG_KEY_n/VALUE_n): never the URL, argv or a file.
func (a Auth) config(repoURL string) [][2]string {
	if a.Token == "" {
		return nil
	}
	return [][2]string{{"http." + repoURL + ".extraHeader", "Authorization: " + a.header()}}
}

// secrets are the strings redacted from errors: the token and its header encoding.
func (a Auth) secrets() []string {
	if a.Token == "" {
		return nil
	}
	return []string{a.Token, strings.TrimPrefix(a.header(), "Basic ")}
}

// Head returns the commit ref points to in repoURL, with git ls-remote (no clone). A ref
// that is already a full commit SHA is returned as is. A branch wins over a tag of the same
// name, and an annotated tag resolves to its commit. Errors wrap ErrGit (or the context's
// error) and carry no credentials.
func Head(ctx context.Context, repoURL, ref string) (string, error) {
	return HeadAuth(ctx, repoURL, ref, Auth{})
}

// HeadAuth is Head on a repository that needs a credential: auth's token goes to git as an
// Authorization header scoped to repoURL (Auth.config), so the URL must be http(s) and carry
// no user info. A zero Auth is Head. The token never appears in an error.
func HeadAuth(ctx context.Context, repoURL, ref string, auth Auth) (_ string, err error) {
	defer func() { err = redactError(err, repoURL, auth.secrets()...) }()
	if repoURL == "" || ref == "" {
		return "", fmt.Errorf("%w: repository URL and ref are required", ErrGit)
	}
	if auth.Token != "" {
		u, perr := url.Parse(repoURL)
		if perr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return "", fmt.Errorf("%w: a token needs an http(s) repository URL without user info", ErrGit)
		}
	}
	if IsCommit(ref) {
		return ref, nil
	}
	out, err := git(ctx, auth.config(repoURL), "ls-remote", "--", repoURL, ref, ref+"^{}")
	if err != nil {
		return "", err
	}
	refs := map[string]string{}
	var matched []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		sha, name, ok := strings.Cut(sc.Text(), "\t")
		if !ok || !commitRE.MatchString(sha) {
			continue
		}
		refs[name] = sha
		matched = append(matched, name)
	}
	for _, name := range []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref, ref} {
		if sha, ok := refs[name]; ok {
			return sha, nil
		}
	}
	if len(matched) == 0 {
		return "", fmt.Errorf("%w ls-remote: ref %q not found in %s", ErrGit, ref, repoURL)
	}
	return "", fmt.Errorf("%w ls-remote: ref %q is not a branch or tag of %s (it matches %s)", ErrGit, ref, repoURL, strings.Join(matched, ", "))
}

// RedactURL drops the user info (a token or password) from a repository URL, for logs and
// status. Other forms (scp-like ssh, plain paths) are returned unchanged.
func RedactURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.User == nil {
		return repoURL
	}
	u.User = url.User("redacted")
	return u.String()
}

// redact replaces repoURL, and its user info on its own, in s.
func redact(s, repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.User == nil {
		return s
	}
	s = strings.ReplaceAll(s, repoURL, RedactURL(repoURL))
	return strings.ReplaceAll(s, u.User.String()+"@", "redacted@")
}

// redactedError is an error whose message had credentials removed; it still unwraps to the
// original, so errors.Is(err, ErrGit) holds.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactError redacts repoURL's user info and each secret from err's message.
func redactError(err error, repoURL string, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := redact(err.Error(), repoURL)
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "redacted")
		}
	}
	if msg != err.Error() {
		return &redactedError{msg: msg, err: err}
	}
	return err
}

// gitConfig is the process-scoped config every command gets: no automatic gc or
// maintenance. A fetch or commit can otherwise leave a detached background gc writing into
// the temporary checkout while it is being removed.
var gitConfig = [][2]string{
	{"gc.auto", "0"},
	{"gc.autoDetach", "false"},
	{"maintenance.auto", "false"},
}

// gitEnv locks git down: no prompts, only allowed transports, no system config, and
// gitConfig.
var gitEnv = environ(nil)

// environ is the locked-down environment with extra config after gitConfig, as
// GIT_CONFIG_COUNT/KEY_n/VALUE_n entries (process-scoped config, never written to a file).
func environ(extra [][2]string) []string {
	pairs := append(slices.Clone(gitConfig), extra...)
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=" + allowedProtocols,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs)),
	}
	for i, kv := range pairs {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	return env
}

// git runs git with a locked-down environment plus config (Auth.config) and returns its
// trimmed stdout.
func git(ctx context.Context, config [][2]string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), environ(config)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w %s: %w", ErrGit, gitVerb(args), ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%w %s: %s", ErrGit, gitVerb(args), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitVerb names the git subcommand for error messages, skipping -C <dir> and -c <kv>.
func gitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C", "-c":
			i++
		default:
			return args[i]
		}
	}
	return ""
}
