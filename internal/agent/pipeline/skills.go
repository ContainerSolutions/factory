package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// nameRE is a plugin or skill name usable as a directory name.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// maxSkillFile bounds one copied skill file; maxPluginManifest a plugin.json.
const (
	maxSkillFile      = 16 << 20
	maxPluginManifest = 1 << 20
)

// skillsURL is a skills source as a URL: sources that aren't URLs are under SkillsBase.
func (r *run) skillsURL(source string) string {
	if strings.Contains(source, "://") {
		return source
	}
	return strings.TrimRight(r.p.o.SkillsBase, "/") + "/" + strings.TrimLeft(source, "/")
}

// materialiseSkills copies the selected skills of each pinned skills repo into skills/, as
// plugin directories (run-pipeline.md §Steps 5): skills/<plugin>/.claude-plugin/plugin.json
// and skills/<plugin>/skills/<skill>/. The harness adapters lay them out for their harness
// in Prepare (Claude Code's --plugin-dir takes the plugin directories as they are; Codex
// copies the skill folders).
func (r *run) materialiseSkills(ctx context.Context) error {
	for i, s := range r.task.Spec.Skills {
		if len(s.Use) == 0 {
			continue
		}
		tree, err := r.skillsTree(ctx, i, s)
		if err != nil {
			return err
		}
		skills, err := materialise(tree, s, r.ws.Skills)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Source, err)
		}
		r.ws.SkillList = append(r.ws.SkillList, skills...)
	}
	return nil
}

// skillsTree returns a checkout of the skills repo at its commit (or ref when the task
// carries no commit): from the cache when the commit is cached, else fetched (into the
// cache when there is one, into the workspace otherwise).
func (r *run) skillsTree(ctx context.Context, i int, s v1alpha1.SkillRef) (string, error) {
	src := r.skillsURL(s.Source)
	want := s.Commit
	if want == "" {
		want = s.Ref
	}
	if want == "" {
		return "", fmt.Errorf("%s: neither a commit nor a ref", s.Source)
	}
	if !validRef(want) {
		return "", fmt.Errorf("%s: %q is not a commit or a ref", s.Source, want)
	}
	cache := r.p.o.SkillsCache
	if cache == "" || !commitRE.MatchString(s.Commit) {
		dir := filepath.Join(r.ws.Tmp, "skills-src-"+strconv.Itoa(i))
		if err := r.fetchTree(ctx, src, want, s.Commit, dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	key := path.Base(strings.TrimSuffix(strings.TrimRight(src, "/"), ".git"))
	if !nameRE.MatchString(key) {
		key = "skills"
	}
	final := filepath.Join(cache, key, s.Commit)
	lk := r.p.skillsLock(final)
	lk.Lock()
	defer lk.Unlock()
	if st, err := os.Stat(final); err == nil && st.IsDir() {
		return final, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return "", fmt.Errorf("skills cache: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(final), s.Commit+".tmp-")
	if err != nil {
		return "", fmt.Errorf("skills cache: %w", err)
	}
	dir := filepath.Join(tmp, "tree")
	if err := r.fetchTree(ctx, src, want, s.Commit, dir); err != nil {
		_ = removeAll(tmp)
		return "", err
	}
	if err := os.Rename(dir, final); err != nil {
		_ = removeAll(tmp)
		return "", fmt.Errorf("skills cache: %w", err)
	}
	_ = removeAll(tmp)
	return final, nil
}

// skillsLock serialises fetches of one cache entry across concurrent runs.
func (p *Pipeline) skillsLock(key string) *sync.Mutex {
	p.skillsMu.Lock()
	defer p.skillsMu.Unlock()
	lk, ok := p.skillsLk[key]
	if !ok {
		lk = &sync.Mutex{}
		p.skillsLk[key] = lk
	}
	return lk
}

// fetchTree fetches ref of src at depth 1 into dir, checks it is commit (when given) and
// drops .git: the tree is plain files.
func (r *run) fetchTree(ctx context.Context, src, ref, commit, dir string) error {
	g := r.git
	steps := [][]string{
		{"init", "--quiet", dir},
		{"-C", dir, "fetch", "--quiet", "--depth", "1", "--no-tags", "--", src, ref},
		{"-C", dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := g.run(ctx, r.ws.Root, args...); err != nil {
			return fmt.Errorf("fetch %s@%s: %w", src, ref, err)
		}
	}
	if commit != "" {
		head, err := g.run(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if !strings.EqualFold(head, commit) && !strings.HasPrefix(strings.ToLower(head), strings.ToLower(commit)) {
			return fmt.Errorf("fetch %s@%s: got commit %s", src, ref, head)
		}
	}
	return removeAll(filepath.Join(dir, ".git"))
}

// plugin is one plugin directory found in a skills tree.
type plugin struct {
	name     string
	dir      string // relative to the tree
	manifest []byte // its .claude-plugin/plugin.json; nil for the tree's root skills
}

// findPlugins lists the plugin directories of a skills tree: every directory (up to three
// levels down) with a .claude-plugin/plugin.json, named by its manifest's name (else the
// directory's). A tree without one at its root still offers skills/<name> at the root, as a
// plugin named after the source repo.
func findPlugins(tree, fallback string) ([]plugin, error) {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	var out []plugin
	seenRoot := false
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if strings.Count(p, "/") > 2 || (p != "." && strings.HasPrefix(d.Name(), ".") && d.Name() != ".claude-plugin") {
			return fs.SkipDir
		}
		if d.Name() != ".claude-plugin" {
			return nil
		}
		dir := path.Dir(p)
		b, err := readLimited(fsys, path.Join(p, "plugin.json"), maxPluginManifest)
		if errors.Is(err, fs.ErrNotExist) {
			return fs.SkipDir
		}
		if err != nil {
			return err
		}
		var m struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(b, &m)
		name := m.Name
		if name == "" {
			name = path.Base(dir)
		}
		if dir == "." {
			seenRoot = true
			if m.Name == "" {
				name = fallback
			}
		}
		out = append(out, plugin{name: name, dir: dir, manifest: b})
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}
	if !seenRoot {
		out = append(out, plugin{name: fallback, dir: "."})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out, nil
}

func readLimited(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", name, limit)
	}
	return b, nil
}

// materialise copies s.Use from tree into dst. A use is "skill" or "plugin:skill"; an
// unqualified skill must be in exactly one plugin.
func materialise(tree string, s v1alpha1.SkillRef, dst string) ([]harness.Skill, error) {
	fallback := path.Base(strings.TrimSuffix(strings.TrimRight(s.Source, "/"), ".git"))
	if !nameRE.MatchString(fallback) {
		fallback = "skills"
	}
	plugins, err := findPlugins(tree, fallback)
	if err != nil {
		return nil, fmt.Errorf("skills tree: %w", err)
	}
	var out []harness.Skill
	written := map[string]bool{}
	for _, use := range s.Use {
		pname, sname, qualified := strings.Cut(use, ":")
		if !qualified {
			pname, sname = "", use
		}
		if !nameRE.MatchString(sname) || (qualified && !nameRE.MatchString(pname)) {
			return nil, fmt.Errorf("skill %q: not a skill name", use)
		}
		var found []plugin
		for _, p := range plugins {
			if qualified && p.name != pname {
				continue
			}
			if st, err := os.Stat(filepath.Join(tree, filepath.FromSlash(p.dir), "skills", sname, "SKILL.md")); err == nil && st.Mode().IsRegular() {
				found = append(found, p)
			}
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("skill %q not found", use)
		case 1:
		default:
			names := make([]string, len(found))
			for i, p := range found {
				names[i] = p.name + ":" + sname
			}
			return nil, fmt.Errorf("skill %q is in several plugins (%s): qualify it", use, strings.Join(names, ", "))
		}
		p := found[0]
		if !nameRE.MatchString(p.name) {
			return nil, fmt.Errorf("plugin %q: not a plugin name", p.name)
		}
		pdir := filepath.Join(dst, p.name)
		if !written[p.name] {
			manifest := p.manifest
			if manifest == nil {
				manifest, _ = json.Marshal(map[string]string{"name": p.name})
			}
			if err := os.MkdirAll(filepath.Join(pdir, ".claude-plugin"), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(pdir, ".claude-plugin", "plugin.json"), manifest, 0o600); err != nil {
				return nil, err
			}
			written[p.name] = true
		}
		sdst := filepath.Join(pdir, "skills", sname)
		if err := copyTree(filepath.Join(tree, filepath.FromSlash(p.dir), "skills", sname), sdst); err != nil {
			return nil, fmt.Errorf("skill %q: %w", use, err)
		}
		out = append(out, harness.Skill{Name: sname, Plugin: p.name, Dir: sdst})
	}
	return out, nil
}

// copyTree copies the regular files and directories under src to dst, keeping the
// executable bit; symlinks and other special files are skipped, so nothing outside src is
// read.
func copyTree(src, dst string) error {
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := readLimited(fsys, p, maxSkillFile)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if info.Mode()&0o111 != 0 {
				mode = 0o700
			}
			return os.WriteFile(target, b, mode)
		}
		return nil // symlinks, devices, sockets
	})
}

// stackUpIfNeeded brings the local stack up when the task needs one (run-pipeline.md §Steps
// 4): it requires has-local-stack and its inputs name versions (train or verifyAgainst).
func (r *run) stackUpIfNeeded(ctx context.Context) error {
	if !containsLabel(r.task, "has-local-stack") {
		return nil
	}
	in := r.task.Spec.Inputs
	if isEmpty(in["train"]) && isEmpty(in["verifyAgainst"]) {
		return nil
	}
	if r.p.o.Stack == nil {
		return errors.New("the task needs the local stack, and this agent has none (configure localDev and the has-local-stack label)")
	}
	env, err := r.p.o.Stack.Up(ctx, r.ws, r.task)
	if err != nil {
		return fmt.Errorf("stack up: %w", err)
	}
	r.stackUp = true
	r.stackEnv = env
	return nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case map[string]any:
		return len(x) == 0
	}
	return false
}
