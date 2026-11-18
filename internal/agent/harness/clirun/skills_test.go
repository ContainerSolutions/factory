package clirun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
)

// TestFlatSkills: each skill's folder is its name, or <plugin>-<name> when two plugins' skills
// share it, and it is what FlattenSkills creates: the folder, with that name in its SKILL.md.
func TestFlatSkills(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		skills [][2]string // plugin, name
		want   []FlatSkill
	}{
		{"none", nil, []FlatSkill{}},
		{"no clash", [][2]string{{"ccf-docs", "docs-style"}, {"", "local"}},
			[]FlatSkill{{"ccf-docs", "docs-style", "docs-style"}, {"", "local", "local"}}},
		{"two plugins", [][2]string{{"ccf-review", "review"}, {"ccf-docs", "docs-style"}, {"lisa", "review"}},
			[]FlatSkill{{"ccf-review", "review", "ccf-review-review"}, {"ccf-docs", "docs-style", "docs-style"}, {"lisa", "review", "lisa-review"}}},
		{"a plugin and none", [][2]string{{"", "review"}, {"lisa", "review"}},
			[]FlatSkill{{"", "review", "review"}, {"lisa", "review", "lisa-review"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src, dst := t.TempDir(), filepath.Join(t.TempDir(), "skills")
			var ws harness.Workspace
			for _, s := range tt.skills {
				dir := filepath.Join(src, s[0], "skills", s[1])
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+s[1]+"\ndescription: d\n---\nbody\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: s[0], Name: s[1], Dir: dir})
			}
			got := FlatSkills(ws.SkillList)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("FlatSkills = %v, want %v", got, tt.want)
			}
			if err := FlattenSkills(ws, dst); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dst)
			if err != nil {
				t.Fatal(err)
			}
			var folders, wantFolders []string
			for _, e := range entries {
				folders = append(folders, e.Name())
			}
			for _, f := range got {
				wantFolders = append(wantFolders, f.Folder)
				b, err := os.ReadFile(filepath.Join(dst, f.Folder, "SKILL.md"))
				if want := "---\nname: " + f.Folder + "\ndescription: d\n---\nbody\n"; err != nil || string(b) != want {
					t.Errorf("%s/SKILL.md = %q (%v), want %q", f.Folder, b, err, want)
				}
			}
			slices.Sort(wantFolders)
			if !slices.Equal(folders, wantFolders) {
				t.Errorf("FlattenSkills made %v, want %v", folders, wantFolders)
			}
		})
	}
}

// TestSetSkillName: the front matter's top-level name is replaced, or added when missing; a
// file without front matter, or whose front matter doesn't end, is left as it is.
func TestSetSkillName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, in, want string
	}{
		{"replaced", "---\ndescription: d\nname: \"review\"\nmeta:\n  name: x\n---\nname: body\n",
			"---\ndescription: d\nname: lisa-review\nmeta:\n  name: x\n---\nname: body\n"},
		{"CRLF", "---\r\nname: review\r\n---\r\nbody\r\n", "---\r\nname: lisa-review\r\n---\r\nbody\r\n"},
		{"added, CRLF", "---\r\n---\r\n", "---\r\nname: lisa-review\r\n---\r\n"},
		{"added", "---\ndescription: d\n---\nbody\n", "---\nname: lisa-review\ndescription: d\n---\nbody\n"},
		{"no front matter", "# review\nname: body\n", "# review\nname: body\n"},
		{"unterminated", "---\ndescription: d\n", "---\ndescription: d\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(p, []byte(tt.in), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := setSkillName(p, "lisa-review"); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != tt.want {
				t.Errorf("SKILL.md = %q (%v), want %q", b, err, tt.want)
			}
		})
	}
}
