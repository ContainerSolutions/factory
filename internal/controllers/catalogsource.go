package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	neturl "net/url"
	"os"
	"strings"

	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/catalog/gitsource"
)

// CatalogSource is where catalog-sync reads the catalog from.
type CatalogSource interface {
	// Head returns the revision the source is at now: for git, the commit the configured
	// ref points to (git ls-remote); for a directory, a hash of its files. It must be cheap
	// enough to call on every poll.
	Head(ctx context.Context) (string, error)
	// Load loads and validates the catalog at revision (as returned by Head). The snapshot's
	// SHA is revision. The diagnostics are catalog.LoadDir's; err is set only when the
	// revision couldn't be read at all.
	Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error)
	// String describes the source for logs and status, without credentials.
	String() string
}

// Catalog-sync source defaults (the LLD's --catalog-* flags). There is no default repository:
// serve requires --catalog-repo or --catalog-dir.
const (
	DefaultCatalogRef  = "main"
	DefaultCatalogPath = "catalog"
)

// GitSource reads the catalog from a git repository through gitsource: Head is
// gitsource.HeadAuth (git ls-remote), Load is gitsource.LoadAuth at that commit (a shallow
// fetch into a temporary directory). gitsource redacts credentials in the URL, and the token,
// from its errors.
type GitSource struct {
	// Repo is the repository URL (https, ssh, git or file). Required: there is no default.
	Repo string
	// Ref is the branch or tag to follow, or a full commit SHA to pin. Default "main".
	Ref string
	// Path is the catalog's directory in the repository. Default "catalog".
	Path string
	// Options are passed to catalog.LoadDir (SHA is set by gitsource).
	Options catalog.Options
	// Token, when set, mints a read-only token for the repository ("owner/name") before each
	// Head and Load, which send it as a header in git's environment, never in the URL or argv.
	// Repo must then be https://<host>/<owner>/<name>[.git]. A mint error fails the call:
	// there is no anonymous fallback. Nil: anonymous.
	Token func(ctx context.Context, repo string) (string, error)

	// headAuth and loadAuth are gitsource.HeadAuth and gitsource.LoadAuth; tests replace them.
	headAuth func(ctx context.Context, repoURL, ref string, auth gitsource.Auth) (string, error)
	loadAuth func(ctx context.Context, repoURL, ref, dir string, opts catalog.Options, auth gitsource.Auth) (*catalog.Catalog, []catalog.Error, error)
}

var _ CatalogSource = GitSource{}

func (g GitSource) withDefaults() GitSource {
	if g.Ref == "" {
		g.Ref = DefaultCatalogRef
	}
	if g.Path == "" {
		g.Path = DefaultCatalogPath
	}
	if g.headAuth == nil {
		g.headAuth = gitsource.HeadAuth
	}
	if g.loadAuth == nil {
		g.loadAuth = gitsource.LoadAuth
	}
	return g
}

// auth is the credential for Repo: a minted token when Token is set, else anonymous.
func (g GitSource) auth(ctx context.Context) (gitsource.Auth, error) {
	if g.Token == nil {
		return gitsource.Auth{}, nil
	}
	repo, ok := RepoOnHost(g.Repo, urlHost(g.Repo))
	if u, err := neturl.Parse(g.Repo); !ok || err != nil || u.Scheme != "https" {
		return gitsource.Auth{}, fmt.Errorf("catalog repository %s: a token needs an https://<host>/<owner>/<name> URL", gitsource.RedactURL(g.Repo))
	}
	tok, err := g.Token(ctx, repo)
	if err == nil && tok == "" {
		err = errors.New("empty token")
	}
	if err != nil {
		return gitsource.Auth{}, fmt.Errorf("mint a read token for the catalog repository %s: %w", repo, err)
	}
	return gitsource.Auth{Token: tok}, nil
}

func (g GitSource) String() string {
	g = g.withDefaults()
	return fmt.Sprintf("git %s ref %s path %s", gitsource.RedactURL(g.Repo), g.Ref, g.Path)
}

// Head resolves Ref with gitsource.HeadAuth (git ls-remote): a branch wins over a tag, an
// annotated tag resolves to its commit, and a full commit SHA is pinned (no token minted).
func (g GitSource) Head(ctx context.Context) (string, error) {
	g = g.withDefaults()
	if gitsource.IsCommit(g.Ref) {
		return g.Ref, nil
	}
	auth, err := g.auth(ctx)
	if err != nil {
		return "", err
	}
	return g.headAuth(ctx, g.Repo, g.Ref, auth)
}

// Load fetches revision and loads the catalog under Path (gitsource.LoadAuth).
func (g GitSource) Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error) {
	g = g.withDefaults()
	auth, err := g.auth(ctx)
	if err != nil {
		return nil, nil, err
	}
	return g.loadAuth(ctx, g.Repo, revision, g.Path, g.Options, auth)
}

// DirSource reads the catalog from a local directory (`--catalog-dir`, local development).
// Head hashes the files LoadDir reads (the same "dir:<sha256>" LoadDir computes), so an edit
// is picked up on the next poll.
type DirSource struct {
	// FS is the catalog root, e.g. os.DirFS(dir).
	FS fs.FS
	// Name describes the directory in logs and status.
	Name string
	// Options are passed to catalog.LoadDir (SHA is set to the revision).
	Options catalog.Options
}

var _ CatalogSource = DirSource{}

// NewDirSource returns a DirSource over the directory dir.
func NewDirSource(dir string, opts catalog.Options) DirSource {
	return DirSource{FS: os.DirFS(dir), Name: dir, Options: opts}
}

func (d DirSource) String() string { return "dir " + d.Name }

// Head hashes the directory: every regular file outside dot-directories that isn't a dot
// file, in lexical order (path, length and content), as LoadDir does.
func (d DirSource) Head(ctx context.Context) (string, error) {
	if d.FS == nil {
		return "", errors.New("dir source: no FS")
	}
	sum := sha256.New()
	err := fs.WalkDir(d.FS, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			if p != "." && strings.HasPrefix(de.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() || strings.HasPrefix(de.Name(), ".") {
			return nil
		}
		data, err := fs.ReadFile(d.FS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", p, len(data))
		sum.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("dir source %s: %w", d.Name, err)
	}
	return "dir:" + hex.EncodeToString(sum.Sum(nil)), nil
}

// Load runs catalog.LoadDir on the directory, labelling the snapshot with revision.
func (d DirSource) Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error) {
	if d.FS == nil {
		return nil, nil, errors.New("dir source: no FS")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opts := d.Options
	opts.SHA = revision
	c, errs := catalog.LoadDir(d.FS, opts)
	return c, errs, nil
}
