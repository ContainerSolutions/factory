// Package gitsource loads a catalog from a git ref: a shallow fetch into a temporary
// directory with the git CLI, then catalog.LoadDir on the catalog's directory in it, with the
// commit as the snapshot's SHA (design/workstreams/dispatcher-core/LLD/catalog.md §Loading).
//
// It is the one part of catalog loading that runs a process and touches the network, so it
// is kept out of the pure catalog package. It shells out to git rather than linking go-git,
// which isn't on the dependency allow-list; the catalog-sync controller (RT) already uses
// git ls-remote.
package gitsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/compliance-framework/factory/internal/catalog"
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
func Load(ctx context.Context, repoURL, ref, dir string, opts catalog.Options) (*catalog.Catalog, []catalog.Error, error) {
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
		if _, err := git(ctx, args...); err != nil {
			return nil, nil, err
		}
	}
	sha, err := git(ctx, "-C", tmp, "rev-parse", "HEAD")
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

// git runs git with a locked-down environment and returns its trimmed stdout.
func git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+allowedProtocols,
		"GIT_CONFIG_NOSYSTEM=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
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
