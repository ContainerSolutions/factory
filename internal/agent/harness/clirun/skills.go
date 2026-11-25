package clirun

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/internal/execfile"
)

// FlattenSkills lays ws's skills out flat in dst, the layout codex and opencode discover: each
// skill folder copied to dst/<folder>/ (FlatSkills names the folders), and dst/<plugin>/bin
// linked to each plugin's bin/ (linkPluginBins).
func FlattenSkills(ws harness.Workspace, dst string) error {
	if err := flattenSkills(ws.SkillList, dst); err != nil {
		return err
	}
	return linkPluginBins(ws, dst)
}

// FlatSkill is one skill in the flat layout: its plugin and name in the workspace, and Folder,
// the folder FlattenSkills copies it to and the name the harness knows it by.
type FlatSkill struct {
	Plugin, Name, Folder string
}

// FlatSkills is skills in the flat layout, in their order: each skill's folder is its name, or
// <plugin>-<name> when two plugins' skills share the name. A harness has no plugins, so an
// adapter can tell the model which name a prompt's <plugin>:<skill> is.
func FlatSkills(skills []harness.Skill) []FlatSkill {
	count := map[string]int{}
	for _, s := range skills {
		count[s.Name]++
	}
	flat := make([]FlatSkill, 0, len(skills))
	for _, s := range skills {
		folder := s.Name
		if count[s.Name] > 1 && s.Plugin != "" {
			folder = s.Plugin + "-" + s.Name
		}
		flat = append(flat, FlatSkill{Plugin: s.Plugin, Name: s.Name, Folder: folder})
	}
	return flat
}

// WriteNew writes b to a new file p (0600); an existing file or symlink at p is an error.
func WriteNew(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// flattenSkills copies each skill folder to dst/<folder>/ (FlatSkills). A skill whose folder
// isn't its name gets the folder as the name in its copied SKILL.md (setSkillName).
func flattenSkills(skills []harness.Skill, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	used := map[string]bool{}
	for i, f := range FlatSkills(skills) {
		name := f.Folder
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || used[name] {
			return fmt.Errorf("skill %q (plugin %q): can't name its folder", f.Name, f.Plugin)
		}
		used[name] = true
		if err := copyTree(skills[i].Dir, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name != f.Name {
			if err := setSkillName(filepath.Join(dst, name, skillFile), name); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

// skillFile is a skill folder's definition.
const skillFile = "SKILL.md"

// setSkillName sets the top-level name in the front matter of the SKILL.md at p (a copy
// flattenSkills made) to name, adding it if missing. opencode knows a skill by that name, not
// by its folder (packages/opencode/src/skill/index.ts at v1.19.6), so two plugins' "review"
// would both be "review" and one would hide the other. A file without front matter is left
// as it is.
func setSkillName(p, name string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(b), "\n")
	if strings.TrimRight(lines[0], "\r\n") != "---" {
		return nil
	}
	for i := 1; i < len(lines); i++ {
		switch l := strings.TrimRight(lines[i], "\r\n"); {
		case strings.HasPrefix(l, "name:"):
			lines[i] = "name: " + name + lines[i][len(l):] // the line's own ending
		case l == "---":
			lines = slices.Insert(lines, 1, "name: "+name+lines[0][len("---"):])
		default:
			continue
		}
		return os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600)
	}
	return nil
}

// linkPluginBins links dst/<plugin>/bin to the pipeline's skills/<plugin>/bin for every plugin
// of ws's skills that has a bin/ directory, unless dst/<plugin>/bin is already there (a skill
// folder of that name with its own bin/). A skill finds another plugin's CLI beside its own
// folder: lisa's self-review runs <lisa skill dir>/../ccf-review/bin/ccfr, which is
// skills/ccf-review/bin/ccfr in the flat layout. A link rather than a copy: ccfr finds its
// lib/ and rules from its real path, the plugin root that stays in the pipeline's skills/.
func linkPluginBins(ws harness.Workspace, dst string) error {
	if ws.Skills == "" {
		return nil
	}
	var done []string
	for _, s := range ws.SkillList {
		p := s.Plugin
		if p == "" || slices.Contains(done, p) {
			continue
		}
		done = append(done, p)
		if strings.ContainsAny(p, `/\`) || p == "." || p == ".." {
			return fmt.Errorf("plugin %q: can't name its folder", p)
		}
		src := filepath.Join(ws.Skills, p, "bin")
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			continue
		}
		link := filepath.Join(dst, p, "bin")
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dst, p), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(src, link); err != nil {
			return fmt.Errorf("plugin %s: link bin/: %w", p, err)
		}
	}
	return nil
}

// copyTree copies the directories and regular files under src to dst (0700 directories,
// files 0700 when executable, else 0600); symlinks and special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if fi.Mode()&0o111 != 0 {
				mode = 0o700
			}
			return copyFile(p, target, mode)
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return execfile.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode, func(out *os.File) error {
		_, err := io.Copy(out, in)
		return err
	})
}
