package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/internal/execfile"
)

// makeWorkspace takes Options.Root and lays the workspace's directories in it, all 0700
// (run-pipeline.md §Steps 1; workspace-run.md §Root), and the `factory` CLI shim in bin/.
// skills/ is made only for FetchSkills, the source that fills it. Cleanup never removes the
// Root.
func (r *run) makeWorkspace(context.Context) error {
	root := r.p.o.Root
	if err := useRoot(root); err != nil {
		return err
	}
	r.ws.Root = root
	ws := harness.Workspace{
		Root:   root,
		Skills: filepath.Join(root, "skills"),
		Out:    filepath.Join(root, "out"),
		Home:   filepath.Join(root, "home"),
		Tmp:    filepath.Join(root, "tmp"),
		Bin:    filepath.Join(root, "bin"),
	}
	ws.Result = filepath.Join(ws.Out, "result.json")
	ws.Schema = filepath.Join(root, "result.schema.json")
	dirs := []string{ws.Out, ws.Home, ws.Tmp, ws.Bin, filepath.Join(root, "ro")}
	if r.p.fetchesSkills() {
		dirs = append([]string{ws.Skills}, dirs...)
	}
	for _, d := range dirs {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
	}
	// `factory` on the harness's PATH is `factory-agent fx` (effector.md §Shape).
	shim := "#!/bin/sh\nexec " + gitcred.ShellQuote(r.p.o.AgentBinary) + " fx \"$@\"\n"
	if err := execfile.WriteFile(filepath.Join(ws.Bin, "factory"), []byte(shim), 0o700); err != nil {
		return fmt.Errorf("workspace: the factory CLI: %w", err)
	}
	ws.WorkDir = root
	r.ws = ws
	return nil
}

// useRoot takes dir as the workspace root (Options.Root): it is created 0700 when absent (its
// parent must exist), and must otherwise be an empty directory, not a symlink. A root that
// already exists keeps its mode, which whoever made it chose.
func useRoot(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		if err := os.Chmod(dir, 0o700); err != nil { // umask-proof
			return fmt.Errorf("workspace root: %w", err)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("workspace root: %w", err)
	}
	f, err := openRootDir(dir)
	if errors.Is(err, errRootNotDir) {
		return fmt.Errorf("workspace root %s is not a directory", dir)
	}
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	if st, err := f.Stat(); err != nil || !st.IsDir() {
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("workspace root: %w", err)
		}
		return fmt.Errorf("workspace root %s is not a directory", dir)
	}
	names, err := f.Readdirnames(1)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	switch {
	case len(names) > 0:
		return fmt.Errorf("workspace root %s is not empty", dir)
	case err != nil && !errors.Is(err, io.EOF):
		return fmt.Errorf("workspace root: %w", err)
	}
	return nil
}

// errRootNotDir is openRootDir's error for a Root that is a symlink or not a directory.
var errRootNotDir = errors.New("not a directory")

// removeAll removes dir, first making read-only directories inside it writable (a Go module
// cache, read-only git objects), so a harness can't make cleanup fail.
func removeAll(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err2 := os.RemoveAll(dir); err2 != nil {
		return errors.Join(err, err2)
	}
	return nil
}
