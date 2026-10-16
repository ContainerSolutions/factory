package gitsource

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/compliance-framework/factory/internal/catalog"
)

// testRepo commits the PoC catalog under catalog/ in a new repository and returns its path
// and commit.
func testRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir = t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "catalog"), os.DirFS("../testdata/poc")); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "--quiet", "--initial-branch=main")
	run("add", ".")
	run("commit", "--quiet", "-m", "catalog")
	sha = run("rev-parse", "HEAD")
	return dir, sha[:len(sha)-1]
}

func TestLoad(t *testing.T) {
	repo, sha := testRepo(t)
	ctx := context.Background()
	url := "file://" + repo

	c, errs, err := Load(ctx, url, "main", "catalog", catalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || catalog.HasErrors(errs) {
		t.Fatalf("catalog %v, diagnostics %v", c != nil, errs)
	}
	if c.SHA != sha {
		t.Errorf("SHA = %q, want the commit %q", c.SHA, sha)
	}

	tests := []struct {
		name, url, ref, dir string
		gitErr              bool
	}{
		{"unknown ref", url, "nope", "catalog", true},
		{"no repository", "file://" + filepath.Join(repo, "missing"), "main", "catalog", true},
		{"ext transport refused", "ext::sh -c touch% /tmp/pwned", "main", "catalog", true},
		{"missing directory", url, "main", "elsewhere", false},
		{"directory outside the repository", url, "main", "../..", false},
		{"no ref", url, "", "catalog", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(ctx, tt.url, tt.ref, tt.dir, catalog.Options{})
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, ErrGit) != tt.gitErr {
				t.Errorf("errors.Is(err, ErrGit) = %v, want %v: %v", !tt.gitErr, tt.gitErr, err)
			}
		})
	}
}

func TestLoadHonoursCancellation(t *testing.T) {
	repo, _ := testRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Load(ctx, "file://"+repo, "main", "catalog", catalog.Options{}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}

// TestLoadRefusesSymlinkEscape: a catalog directory that is a symlink out of the repository
// (e.g. to the dispatcher host's deploy/poc) must not be read.
func TestLoadRefusesSymlinkEscape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	outside := t.TempDir()
	if err := os.CopyFS(outside, os.DirFS("../testdata/poc")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "catalog")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "."},
		{"commit", "--quiet", "-m", "symlinked catalog"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	c, _, err := Load(context.Background(), "file://"+dir, "main", "catalog", catalog.Options{})
	if err == nil || c != nil {
		t.Fatalf("Load followed a symlink out of the checkout: catalog %v, err %v", c != nil, err)
	}
}
