package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
)

// AgentShimOp is the `factory-agent fx` op behind the workspace's bin/factory-agent: the
// factory-agent a run sees, with only `stack env` and `stack wait` (run-pipeline.md
// §Environment, local-stack.md).
const AgentShimOp = "agent-shim"

// makeWorkspace creates workRoot/<taskID>/ and its directories, all 0700
// (run-pipeline.md §Steps 1), and the CLI shims in bin/: `factory` and `factory-agent`.
func (r *run) makeWorkspace(context.Context) error {
	if err := os.MkdirAll(r.p.o.WorkRoot, 0o700); err != nil {
		return fmt.Errorf("work root: %w", err)
	}
	root := filepath.Join(r.p.o.WorkRoot, r.id)
	// A leftover from a crashed run of the same task is replaced.
	if _, err := os.Lstat(root); err == nil {
		r.log.Warn("removing a leftover workspace", "dir", root)
		if err := removeAll(root); err != nil {
			return fmt.Errorf("leftover workspace: %w", err)
		}
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	// Set before anything else can fail, so cleanup removes it.
	r.ws.Root = root
	if err := os.Chmod(root, 0o700); err != nil { // umask-proof
		return fmt.Errorf("workspace: %w", err)
	}
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
	for _, d := range []string{ws.Skills, ws.Out, ws.Home, ws.Tmp, ws.Bin, filepath.Join(root, "ro")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
	}
	// `factory` on the harness's PATH is `factory-agent fx` (effector.md §Shape), and
	// `factory-agent` is `factory-agent fx agent-shim`, which skills call for the run's stack
	// (`factory-agent stack env|wait`): the owner's PATH needn't have the agent binary.
	agent := gitcred.ShellQuote(r.p.o.AgentBinary)
	for _, s := range []struct{ name, args string }{
		{"factory", "fx"},
		{"factory-agent", "fx " + AgentShimOp},
	} {
		shim := "#!/bin/sh\nexec " + agent + " " + s.args + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(ws.Bin, s.name), []byte(shim), 0o700); err != nil {
			return fmt.Errorf("workspace: the %s CLI: %w", s.name, err)
		}
	}
	ws.WorkDir = root
	r.ws = ws
	return nil
}

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
