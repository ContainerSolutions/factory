package harnesstest_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Bundle writes a bundle runbundle.Read accepts, with the task's skills and its git access,
// and no token in any file.
func TestBundle(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"claude-code", "codex"} {
		t.Run(h, func(t *testing.T) {
			t.Parallel()
			e := harnesstest.NewE2E(t, "tsk_bundle", "Write the docs.\n")
			dir, job := e.Bundle(v1alpha1.HarnessResolved{Name: h, Model: "m"}, []string{"pr-opened"})
			b, err := runbundle.Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			m := b.Manifest
			if m.TaskID != "tsk_bundle" || m.RunID != "run_1" || m.LeaseID != "lse_1" || string(b.Prompt) != "Write the docs.\n" ||
				b.OutputSchema != nil || len(m.Context) != 0 || b.CAFile == "" {
				t.Errorf("manifest %+v", m)
			}
			wantGit := runbundle.GitAccess{Provider: "github-public", Host: "github.com", Repo: "acme/docs", Permissions: map[string]string{"contents": "write"}}
			if !reflect.DeepEqual(m.Git, wantGit) {
				t.Errorf("git access %+v", m.Git)
			}
			want := []runbundle.Skill{
				{Name: "docs-style", Plugin: "ccf-docs", Dir: "skills/ccf-docs/skills/docs-style"},
				{Name: "review", Plugin: "ccf-review", Dir: "skills/ccf-review/skills/review"},
				{Name: "review", Plugin: "lisa", Dir: "skills/lisa/skills/review"},
			}
			if !reflect.DeepEqual(m.Skills, want) {
				t.Errorf("skills %+v", m.Skills)
			}
			if (h == "codex") != (m.Codex != nil && *m.Codex == harnesstest.CodexAccount) {
				t.Errorf("codex account %+v", m.Codex)
			}
			tokens := [][]byte{[]byte(harnesstest.GitToken), []byte(harnesstest.AgentToken), []byte(job.Git.Get().Token)}
			n := 0
			err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				n++
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				for _, tok := range tokens {
					if bytes.Contains(data, tok) {
						t.Errorf("%s holds a token", p)
					}
				}
				if runbundle.LooksLikeToken(string(data)) {
					t.Errorf("%s holds a value shaped like a token", p)
				}
				return nil
			})
			if err != nil || n < 5 {
				t.Errorf("walked %d files: %v", n, err)
			}
		})
	}
}
