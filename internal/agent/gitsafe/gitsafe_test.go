package gitsafe_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/gitsafe"
	"github.com/compliance-framework/factory/internal/agent/gitsafe/gitsafetest"
)

func TestArgs(t *testing.T) {
	t.Parallel()
	got := gitsafe.Args("push", "origin")
	want := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "push", "origin"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

func TestEnv(t *testing.T) {
	t.Parallel()
	ours := func(n string, m string) []string {
		return []string{"GIT_CONFIG_KEY_" + n + "=core.hooksPath", "GIT_CONFIG_VALUE_" + n + "=/dev/null",
			"GIT_CONFIG_KEY_" + m + "=core.fsmonitor", "GIT_CONFIG_VALUE_" + m + "=false"}
	}
	helper := []string{"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=!helper", "GIT_CONFIG_KEY_2=credential.useHttpPath", "GIT_CONFIG_VALUE_2=true"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	for _, tt := range []struct {
		name string
		env  []string
		want []string
	}{
		{"none", []string{"PATH=/bin"}, cat([]string{"PATH=/bin"}, ours("0", "1"), []string{"GIT_CONFIG_COUNT=2"})},
		{"after the helper's",
			cat([]string{"PATH=/bin", "GIT_CONFIG_COUNT=3"}, helper),
			cat([]string{"PATH=/bin"}, helper, ours("3", "4"), []string{"GIT_CONFIG_COUNT=5"})},
		{"stale entries past the count are replaced",
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/planted", "GIT_CONFIG_KEY_9=x.y"},
			cat([]string{"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c", "GIT_CONFIG_KEY_9=x.y"}, ours("1", "2"), []string{"GIT_CONFIG_COUNT=3"})},
		{"the last count wins",
			[]string{"GIT_CONFIG_COUNT=7", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"},
			cat([]string{"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"}, ours("1", "2"), []string{"GIT_CONFIG_COUNT=3"})},
		{"an unparsable count is 0", []string{"GIT_CONFIG_COUNT=x"}, cat(ours("0", "1"), []string{"GIT_CONFIG_COUNT=2"})},
		{"a negative count is 0", []string{"GIT_CONFIG_COUNT=-1"}, cat(ours("0", "1"), []string{"GIT_CONFIG_COUNT=2"})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := slices.Clone(tt.env)
			if got := gitsafe.Env(tt.env); !slices.Equal(got, tt.want) {
				t.Errorf("Env =\n%q\nwant\n%q", got, tt.want)
			}
			if !slices.Equal(in, tt.env) {
				t.Error("Env modified its argument")
			}
		})
	}
}

// baseEnv is a clean git environment with one process-scoped pair already set, as the
// credential helper's are.
func baseEnv(t *testing.T) []string {
	home := t.TempDir()
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=factory.fromenv", "GIT_CONFIG_VALUE_0=kept"}
}

func git(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPlantedCodeDoesNotRun: in a checkout armed by gitsafetest, plain git runs the planted
// programs (the fixture is live) and git with Args or Env runs none, while keeping the
// environment's own pairs.
func TestPlantedCodeDoesNotRun(t *testing.T) {
	t.Parallel()
	for _, v := range []struct {
		name string
		v    gitsafetest.Vector
		live []string // what plain git runs (a prefix of each)
	}{
		{"hooks dir", gitsafetest.HooksDir, []string{"hooks:"}},
		{"core.hooksPath", gitsafetest.HooksPath, []string{"hooksPath:"}},
		{"core.fsmonitor", gitsafetest.FSMonitor, []string{"fsmonitor"}},
		{"all", gitsafetest.All, []string{"hooksPath:", "fsmonitor"}}, // core.hooksPath shadows .git/hooks
	} {
		for _, mode := range []string{"plain", "args", "env"} {
			t.Run(v.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				env := baseEnv(t)
				bare := filepath.Join(t.TempDir(), "app.git")
				git(t, env, ".", "init", "--quiet", "--bare", "-b", "main", bare)
				repo := filepath.Join(t.TempDir(), "repo")
				git(t, env, ".", "clone", "--quiet", bare, repo)
				git(t, env, repo, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "initial")
				git(t, env, repo, "push", "--quiet", "origin", "main")
				trap := gitsafetest.Plant(t, repo, v.v)

				run := func(args ...string) string {
					t.Helper()
					switch mode {
					case "args":
						return git(t, env, repo, gitsafe.Args(args...)...)
					case "env":
						return git(t, gitsafe.Env(env), repo, args...)
					}
					return git(t, env, repo, args...)
				}
				run("checkout", "--quiet", "-b", "topic")
				if err := os.WriteFile(filepath.Join(repo, "a.md"), []byte("a\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				run("add", "a.md")
				run("commit", "--quiet", "--no-gpg-sign", "-m", "a")
				run("status", "--porcelain")
				run("push", "--quiet", "origin", "topic")
				run("fetch", "--quiet", "origin")
				run("rebase", "--quiet", "origin/main")
				run("diff", "--name-only", "--diff-filter=U")
				if got := run("config", "factory.fromenv"); got != "kept" {
					t.Errorf("the environment's own pair: %q", got)
				}

				fired := trap.Fired(t)
				if mode == "plain" {
					for _, l := range v.live {
						if !slices.ContainsFunc(fired, func(f string) bool { return strings.HasPrefix(f, l) }) {
							t.Errorf("plain git didn't run the planted %s* (the fixture is dead): %v", l, fired)
						}
					}
					return
				}
				if len(fired) > 0 {
					t.Errorf("planted code ran: %v", fired)
				}
			})
		}
	}
}
