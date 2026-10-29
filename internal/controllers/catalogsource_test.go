package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/catalog/gitsource"
)

const pocCatalog = "../catalog/testdata/poc"

// brokenRule is a file that fails the catalog: invalid YAML.
const brokenRule = "apiVersion: factory.ccf.dev/v1alpha1\nkind: Rule\nmetadata: [unclosed\n"

func hasCode(errs []catalog.Error, code, file string) bool {
	return slices.ContainsFunc(errs, func(e catalog.Error) bool { return e.Code == code && e.File == file })
}

// TestCatalogSyncDirEndToEnd runs catalog-sync over the real loader on a copy of the PoC
// catalog: good, then corrupted (kept old), then repaired.
func TestCatalogSyncDirEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocCatalog)); err != nil {
		t.Fatal(err)
	}
	src := NewDirSource(dir, catalog.Options{})
	s, clk := newTestSync(t, src, CatalogSyncOptions{})

	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync(good) = %v; status %+v", err, s.Status())
	}
	good := s.Current()
	if good == nil || len(good.RuleSets) == 0 || !strings.HasPrefix(good.SHA, "dir:") {
		t.Fatalf("live catalog %+v", good)
	}
	// Head hashes exactly what LoadDir hashes.
	if c, errs := catalog.LoadDir(os.DirFS(dir), catalog.Options{}); c == nil || c.SHA != good.SHA {
		t.Fatalf("LoadDir's own SHA %v differs from Head's %s (%v)", c, good.SHA, errs)
	}

	broken := filepath.Join(dir, "rules", "zz-broken.yaml")
	if err := os.WriteFile(broken, []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := clk.Advance(DefaultCatalogPollInterval)
	err := s.Sync(ctx, false)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(broken) = %v, want ErrCatalogInvalid", err)
	}
	if s.Current() != good {
		t.Fatal("the live catalog changed after a bad load")
	}
	st := s.Status()
	if st.LastGoodSHA != good.SHA || st.LastAttemptSHA == good.SHA || !strings.HasPrefix(st.LastAttemptSHA, "dir:") ||
		!st.LastFailure.Equal(t1) || !st.LastSuccess.Equal(t0) || !hasCode(st.Errors, catalog.CodeSyntax, "rules/zz-broken.yaml") {
		t.Fatalf("status = %+v", st)
	}

	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync(repaired) = %v", err)
	}
	if st := s.Status(); s.Current() != good || st.LastAttemptSHA != good.SHA || st.Errors != nil {
		t.Fatalf("after repair (same content as the live catalog): %+v", st)
	}
}

// testRepo commits the PoC catalog under catalog/ in a new repository on main and returns
// its path, a git runner for it, and the commit.
func testRepo(t *testing.T) (dir string, git func(args ...string) string, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir = t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "catalog"), os.DirFS(pocCatalog)); err != nil {
		t.Fatal(err)
	}
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0", "GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	git("add", ".")
	git("commit", "--quiet", "-m", "catalog")
	return dir, git, git("rev-parse", "HEAD")
}

func TestCatalogSyncGitEndToEnd(t *testing.T) {
	ctx := context.Background()
	repo, git, sha := testRepo(t)
	src := GitSource{Repo: "file://" + repo, Ref: "main", Path: "catalog"}
	s, _ := newTestSync(t, src, CatalogSyncOptions{})

	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync = %v; status %+v", err, s.Status())
	}
	if c := s.Current(); c == nil || c.SHA != sha {
		t.Fatalf("live catalog %v, want commit %s", c, sha)
	}

	if err := os.WriteFile(filepath.Join(repo, "catalog", "rules", "zz-broken.yaml"), []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "break the catalog")
	bad := git("rev-parse", "HEAD")

	if err := s.Sync(ctx, false); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(bad commit) = %v, want ErrCatalogInvalid", err)
	}
	st := s.Status()
	if s.Current().SHA != sha || st.LastGoodSHA != sha || st.LastAttemptSHA != bad || !hasCode(st.Errors, catalog.CodeSyntax, "rules/zz-broken.yaml") {
		t.Fatalf("status = %+v", st)
	}
}

func TestGitSourceRedactsCredentials(t *testing.T) {
	g := GitSource{Repo: "https://x-access-token:s3cr3t@github.com/ContainerSolutions/factory"}
	if s := g.String(); strings.Contains(s, "s3cr3t") || !strings.Contains(s, "github.com/ContainerSolutions/factory") {
		t.Errorf("String() = %q", s)
	}
}

// TestGitSourceToken: with Token set, Head and Load mint a token for the repository and pass
// it to gitsource; a mint error or a URL a token can't go with fails, with no anonymous call.
func TestGitSourceToken(t *testing.T) {
	const tok = "ghs_catalogT0ken"
	const pinned = "0123456789abcdef0123456789abcdef01234567"
	mint := func(_ context.Context, repo string) (string, error) {
		if repo != "compliance-framework/factory-catalog" {
			return "", errors.New("unexpected repo " + repo)
		}
		return tok, nil
	}
	tests := []struct {
		name    string
		repo    string
		ref     string
		token   func(context.Context, string) (string, error)
		want    gitsource.Auth // what gitsource gets
		wantErr string         // "": no error
		noCall  bool           // gitsource must not be called
	}{
		{name: "minted", repo: "https://github.com/compliance-framework/factory-catalog", token: mint, want: gitsource.Auth{Token: tok}},
		{name: "minted, .git URL", repo: "https://github.com/compliance-framework/factory-catalog.git", token: mint, want: gitsource.Auth{Token: tok}},
		{name: "no Token: anonymous", repo: "https://github.com/compliance-framework/factory-catalog"},
		{name: "mint error", repo: "https://github.com/compliance-framework/factory-catalog", token: func(context.Context, string) (string, error) {
			return "", errors.New("no installation")
		}, wantErr: "mint a read token for the catalog repository compliance-framework/factory-catalog: no installation", noCall: true},
		{name: "empty token", repo: "https://github.com/compliance-framework/factory-catalog", token: func(context.Context, string) (string, error) {
			return "", nil
		}, wantErr: "empty token", noCall: true},
		{name: "ssh URL", repo: "git@github.com:compliance-framework/factory-catalog.git", token: mint, wantErr: "a token needs an https://", noCall: true},
		{name: "http URL", repo: "http://github.com/compliance-framework/factory-catalog", token: mint, wantErr: "a token needs an https://", noCall: true},
		{name: "not a repo path", repo: "https://github.com/compliance-framework", token: mint, wantErr: "a token needs an https://", noCall: true},
		{name: "user info", repo: "https://x:s3cr3t@github.com/compliance-framework/factory-catalog", token: mint, wantErr: "a token needs an https://", noCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var heads, loads []gitsource.Auth
			g := GitSource{
				Repo: tt.repo, Ref: tt.ref, Token: tt.token,
				headAuth: func(_ context.Context, _, _ string, auth gitsource.Auth) (string, error) {
					heads = append(heads, auth)
					return pinned, nil
				},
				loadAuth: func(_ context.Context, _, ref, _ string, _ catalog.Options, auth gitsource.Auth) (*catalog.Catalog, []catalog.Error, error) {
					loads = append(loads, auth)
					return &catalog.Catalog{SHA: ref}, nil, nil
				},
			}
			_, herr := g.Head(context.Background())
			_, _, lerr := g.Load(context.Background(), pinned)
			for _, err := range []error{herr, lerr} {
				if tt.wantErr == "" && err != nil {
					t.Fatalf("err = %v", err)
				}
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), "s3cr3t") {
					t.Errorf("the error carries a credential: %v", err)
				}
			}
			if tt.noCall {
				if len(heads)+len(loads) != 0 {
					t.Fatalf("gitsource called (heads %v, loads %v), want no anonymous fallback", heads, loads)
				}
				return
			}
			if !slices.Equal(heads, []gitsource.Auth{tt.want}) || !slices.Equal(loads, []gitsource.Auth{tt.want}) {
				t.Errorf("gitsource got heads %v, loads %v; want %v each", heads, loads, tt.want)
			}
		})
	}
}

// TestGitSourcePinnedHead: a pinned commit is Head's answer without a mint or an ls-remote.
func TestGitSourcePinnedHead(t *testing.T) {
	const pinned = "0123456789abcdef0123456789abcdef01234567"
	g := GitSource{
		Repo: "https://github.com/compliance-framework/factory-catalog", Ref: pinned,
		Token: func(context.Context, string) (string, error) { return "", errors.New("minted") },
		headAuth: func(context.Context, string, string, gitsource.Auth) (string, error) {
			return "", errors.New("ls-remote")
		},
	}
	if got, err := g.Head(context.Background()); err != nil || got != pinned {
		t.Errorf("Head = %q, %v; want %s", got, err, pinned)
	}
}

func TestDefaults(t *testing.T) {
	g := GitSource{}.withDefaults()
	if g.Repo != DefaultCatalogRepo || g.Ref != "main" || g.Path != "catalog" {
		t.Errorf("GitSource defaults = %+v", g)
	}
	if _, err := (DirSource{}).Head(context.Background()); err == nil {
		t.Error("DirSource without FS: want an error")
	}
}
