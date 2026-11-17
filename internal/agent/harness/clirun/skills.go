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
// skill folder copied to dst/<name>/ (two plugins' skills of the same name both go in as
// <plugin>-<name>), and dst/<plugin>/bin linked to each plugin's bin/ (linkPluginBins).
func FlattenSkills(ws harness.Workspace, dst string) error {
	if err := flattenSkills(ws.SkillList, dst); err != nil {
		return err
	}
	return linkPluginBins(ws, dst)
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

// flattenSkills copies each skill folder to dst/<name>/. Two plugins' skills of the same name
// both go in as <plugin>-<name>.
func flattenSkills(skills []harness.Skill, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	count := map[string]int{}
	for _, s := range skills {
		count[s.Name]++
	}
	used := map[string]bool{}
	for _, s := range skills {
		name := s.Name
		if count[name] > 1 && s.Plugin != "" {
			name = s.Plugin + "-" + s.Name
		}
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || used[name] {
			return fmt.Errorf("skill %q (plugin %q): can't name its folder", s.Name, s.Plugin)
		}
		used[name] = true
		if err := copyTree(s.Dir, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
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
