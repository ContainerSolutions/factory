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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/internal/execfile"
	"github.com/compliance-framework/factory/internal/agent/runner"
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
func (r *run) skillsURL(source string) string { return r.p.skillsURL(source) }

// skillsURL is the pipeline's: the run's and PrepareBundle's.
func (p *Pipeline) skillsURL(source string) string {
	if strings.Contains(source, "://") {
		return source
	}
	return strings.TrimRight(p.o.SkillsBase, "/") + "/" + strings.TrimLeft(source, "/")
}

// SkillSource gives a run its materialised skills (workspace-run.md §Sources): dir is the
// workspace's Skills directory (the plugin directories) and list the selected skills, their
// Dir absolute under dir. dst is the path of the workspace's own skills/, which the pipeline
// creates only for FetchSkills: another source that fills it creates it (0700) and returns
// it; a source with the skills elsewhere returns that directory instead.
type SkillSource interface {
	Skills(ctx context.Context, job *runner.Job, dst string) (dir string, list []harness.Skill, err error)
}

// FetchSkills is the default SkillSource: each pinned skills repo is fetched with the run's
// git (through the skills cache, by commit) and the selected skills are materialised into
// the workspace's skills/ (run-pipeline.md §Steps 5). It needs the run's git, so only the
// pipeline runs it: its Skills method, called on its own, is an error.
type FetchSkills struct{}

var _ SkillSource = FetchSkills{}

// errFetchSkillsAlone is FetchSkills called outside a run.
var errFetchSkillsAlone = errors.New("FetchSkills runs only as a pipeline's skills source")

// Skills implements SkillSource. The pipeline runs FetchSkills itself, with the run's git and
// the skills cache; called on its own it fails.
func (FetchSkills) Skills(context.Context, *runner.Job, string) (string, []harness.Skill, error) {
	return "", nil, errFetchSkillsAlone
}

// PrebuiltSkills is the SkillSource of skills materialised before the run: a run bundle's
// skills/ (Dir) and its manifest's list (List, each Dir absolute under Dir), returned as they
// are. The workspace's Skills is then Dir, which the adapters only read.
type PrebuiltSkills struct {
	Dir  string
	List []harness.Skill
}

var _ SkillSource = PrebuiltSkills{}

// Skills implements SkillSource.
func (s PrebuiltSkills) Skills(context.Context, *runner.Job, string) (string, []harness.Skill, error) {
	if s.Dir == "" && len(s.List) > 0 {
		return "", nil, errors.New("prebuilt skills without their directory")
	}
	return s.Dir, slices.Clone(s.List), nil
}

// materialiseSkills fills the workspace's skills from the run's SkillSource: with
// FetchSkills, the run's skillFetcher copies the selected skills of each pinned skills repo
// into skills/, as plugin directories (run-pipeline.md §Steps 5); any other source gives the
// directory and the list itself.
func (r *run) materialiseSkills(ctx context.Context) error {
	if r.p.fetchesSkills() {
		list, err := r.p.skillFetcher(r.git, r.ws.Root, r.ws.Tmp).fetch(ctx, r.task.Spec.Skills, r.ws.Skills)
		r.ws.SkillList = append(r.ws.SkillList, list...)
		return err
	}
	dir, list, err := r.p.o.Skills.Skills(ctx, r.job, r.ws.Skills)
	if err != nil {
		return err
	}
	r.ws.Skills, r.ws.SkillList = dir, list
	return nil
}

// fetchesSkills reports whether the pipeline's SkillSource is FetchSkills (a value or a
// pointer): the run fetches the skills into the workspace's skills/ itself.
func (p *Pipeline) fetchesSkills() bool {
	switch p.o.Skills.(type) {
	case FetchSkills, *FetchSkills:
		return true
	}
	return false
}

// skillFetcher fetches pinned skills repos and materialises their selected skills, with no
// workspace of its own: a run's (its git, its root and tmp/) or PrepareBundle's scratch one.
type skillFetcher struct {
	p   *Pipeline
	git *gitRunner
	cwd string // where git runs
	tmp string // uncached checkouts: tmp/skills-src-<i>
}

// skillFetcher is a fetcher with g, running git in cwd and keeping uncached checkouts in tmp;
// the cache, its per-entry lock and SkillsBase are the pipeline's.
func (p *Pipeline) skillFetcher(g *gitRunner, cwd, tmp string) *skillFetcher {
	return &skillFetcher{p: p, git: g, cwd: cwd, tmp: tmp}
}

// fetch copies the selected skills of each pinned skills repo into dst, as plugin directories
// (run-pipeline.md §Steps 5): dst/<plugin>/.claude-plugin/plugin.json, the plugin's other root
// files and directories (bin/, rules/, …) and dst/<plugin>/skills/<skill>/ for the selected
// skills only. The harness adapters lay them out for their harness in Prepare (Claude Code's
// --plugin-dir takes the plugin directories as they are; Codex copies the skill folders), and
// each plugin's bin/ is on the harness's PATH. It returns the skills materialised so far, also
// on an error.
func (f *skillFetcher) fetch(ctx context.Context, refs []v1alpha1.SkillRef, dst string) ([]harness.Skill, error) {
	var out []harness.Skill
	for i, s := range refs {
		if len(s.Use) == 0 {
			continue
		}
		tree, err := f.tree(ctx, i, s)
		if err != nil {
			return out, err
		}
		skills, err := materialise(tree, s, dst)
		if err != nil {
			return out, fmt.Errorf("%s: %w", s.Source, err)
		}
		out = append(out, skills...)
	}
	return out, nil
}

// tree returns a checkout of the skills repo at its commit (or ref when the task carries no
// commit): from the cache when the commit is cached, else fetched (into the cache when there
// is one, into tmp otherwise).
func (f *skillFetcher) tree(ctx context.Context, i int, s v1alpha1.SkillRef) (string, error) {
	src := f.p.skillsURL(s.Source)
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
	cache := f.p.o.SkillsCache
	if cache == "" || !commitRE.MatchString(s.Commit) {
		dir := filepath.Join(f.tmp, "skills-src-"+strconv.Itoa(i))
		if err := f.fetchTree(ctx, src, want, s.Commit, dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	key := path.Base(strings.TrimSuffix(strings.TrimRight(src, "/"), ".git"))
	if !nameRE.MatchString(key) {
		key = "skills"
	}
	final := filepath.Join(cache, key, s.Commit)
	lk := f.p.skillsLock(final)
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
	if err := f.fetchTree(ctx, src, want, s.Commit, dir); err != nil {
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
func (f *skillFetcher) fetchTree(ctx context.Context, src, ref, commit, dir string) error {
	g := f.git
	steps := [][]string{
		{"init", "--quiet", dir},
		{"-C", dir, "fetch", "--quiet", "--depth", "1", "--no-tags", "--", src, ref},
		{"-C", dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := g.run(ctx, f.cwd, args...); err != nil {
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
// unqualified skill must be in exactly one plugin. Each plugin a use names gets its whole
// tree (bin/, lib/, rules/, docs/, …) except skills/, where only the selected skills go, its
// Claude-only components (claudeOnlyPluginFiles) and other plugins' directories nested in it; its files are bounded together by
// maxPluginBytes and maxPluginFiles.
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
	budgets := map[string]*budget{}
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
		b := budgets[p.name]
		if b == nil {
			b = pluginBudget()
			budgets[p.name] = b
			if err := writePlugin(tree, p, plugins, pdir, b); err != nil {
				return nil, fmt.Errorf("plugin %q: %w", p.name, err)
			}
		}
		sdst := filepath.Join(pdir, "skills", sname)
		if err := copyTree(filepath.Join(tree, filepath.FromSlash(p.dir), "skills", sname), sdst, nil, b); err != nil {
			return nil, fmt.Errorf("skill %q: %w", use, err)
		}
		out = append(out, harness.Skill{Name: sname, Plugin: p.name, Dir: sdst})
	}
	return out, nil
}

// Bounds of one materialised plugin, its root files and selected skills together.
const (
	maxPluginBytes = 50 << 20
	maxPluginFiles = 5000
)

// budget counts what has been copied of one plugin against its bounds.
type budget struct {
	maxFiles int
	maxBytes int64
	files    int
	bytes    int64
}

// pluginBudget is a budget of maxPluginFiles and maxPluginBytes.
func pluginBudget() *budget { return &budget{maxFiles: maxPluginFiles, maxBytes: maxPluginBytes} }

// take counts one more file or directory of size bytes, or refuses it past a bound.
func (b *budget) take(name string, size int64) error {
	b.files++
	b.bytes += size
	if b.files > b.maxFiles {
		return fmt.Errorf("%s: the plugin is over %d files and directories", name, b.maxFiles)
	}
	if b.bytes > b.maxBytes {
		return fmt.Errorf("%s: the plugin is over %d bytes", name, b.maxBytes)
	}
	return nil
}

// claudeOnlyPluginFiles are the plugin root's Claude Code components left out of a run (the
// default locations of the plugins reference, code.claude.com/docs/en/plugins-reference
// §Standard layout): hooks, slash commands, subagents, MCP and LSP servers, output styles,
// monitors, workflows, themes and plugin settings. Runs never loaded them, ccf-skills'
// HEADLESS.md bans hooks, slash commands and subagents in the headless path, and a plugin's
// hooks, servers, monitors or workflows would run code in the run beyond the factory's own hook
// and the Effector's MCP server (the adapter writes the run's settings, hook and MCP config).
var claudeOnlyPluginFiles = []string{
	"hooks", "hooks.json", "commands", "agents", ".mcp.json", ".lsp.json", "output-styles",
	"monitors", "workflows", "themes", "settings.json",
}

// manifestKeys are the plugin.json keys a run keeps (code.claude.com/docs/en/plugins-reference
// §Fields): the identity fields only. Every other key is dropped: the component keys (hooks,
// mcpServers, lspServers, commands, agents, outputStyles, experimental.monitors, …), since they
// would declare inline, or at other paths, what claudeOnlyPluginFiles leaves out; and skills,
// whose extra scan paths would load skills that weren't selected from the copied tree (the
// selected ones are under skills/, which Claude Code scans by default).
var manifestKeys = []string{"name", "version", "description", "author", "homepage", "repository", "license", "keywords"}

// reduceManifest is manifest with only manifestKeys, as found; a manifest that isn't a JSON
// object is refused.
func reduceManifest(manifest []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &m); err != nil || m == nil {
		return nil, errors.New(".claude-plugin/plugin.json is not a JSON object")
	}
	out := map[string]json.RawMessage{}
	for _, k := range manifestKeys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// writePlugin lays out plugin p of tree in pdir: its manifest (reduced to manifestKeys, or
// generated for a plugin without one) and its whole tree but .claude-plugin/, skills/ (the
// selected skills are copied one by one), claudeOnlyPluginFiles and the directories of the
// other plugins nested in it (a plugin at the tree's root holds the others).
func writePlugin(tree string, p plugin, all []plugin, pdir string, b *budget) error {
	var manifest []byte
	if p.manifest == nil {
		manifest, _ = json.Marshal(map[string]string{"name": p.name})
	} else {
		var err error
		if manifest, err = reduceManifest(p.manifest); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(pdir, ".claude-plugin"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pdir, ".claude-plugin", "plugin.json"), manifest, 0o600); err != nil {
		return err
	}
	skip := map[string]bool{".claude-plugin": true, "skills": true}
	for _, f := range claudeOnlyPluginFiles {
		skip[f] = true
	}
	for _, q := range all {
		switch {
		case q.dir == p.dir:
		case p.dir == ".":
			skip[q.dir] = true
		case strings.HasPrefix(q.dir, p.dir+"/"):
			skip[strings.TrimPrefix(q.dir, p.dir+"/")] = true
		}
	}
	return copyTree(filepath.Join(tree, filepath.FromSlash(p.dir)), pdir, skip, b)
}

// copyTree copies the regular files and directories under src to dst but the paths (relative
// to src, slash-separated) in skip, keeping the executable bit and counting each against b;
// symlinks are skipped, so nothing outside src is read, and other special files (devices,
// pipes, sockets) are refused.
func copyTree(src, dst string, skip map[string]bool, b *budget) error {
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
		if skip[p] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			if p != "." {
				if err := b.take(p, 0); err != nil {
					return err
				}
			}
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := b.take(p, info.Size()); err != nil {
				return err
			}
			data, err := readLimited(fsys, p, maxSkillFile)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if info.Mode()&0o111 != 0 {
				mode = 0o700
			}
			return execfile.WriteFile(target, data, mode)
		case d.Type()&fs.ModeSymlink != 0:
			return nil
		}
		return fmt.Errorf("%s: a special file (%s)", p, d.Type())
	})
}
