package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookMain(t *testing.T) {
	root := t.TempDir()
	out, repo, ro := filepath.Join(root, "out"), filepath.Join(root, "repo"), filepath.Join(root, "ro")
	for _, d := range []string{out, repo, ro} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A link in out/ to the read-only checkout doesn't let a write through.
	if err := os.Symlink(ro, filepath.Join(out, "escape")); err != nil {
		t.Fatal(err)
	}
	call := func(tool string, input map[string]any, cwd string) string {
		b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": input, "cwd": cwd, "hook_event_name": "PreToolUse"})
		return string(b)
	}
	readWrite := []string{"pre-tool-use", "--write", out, "--write", repo}
	readOnly := []string{"pre-tool-use", "--write", out}
	tests := []struct {
		name  string
		args  []string
		input string
		deny  string // "" allows
	}{
		{"write the result", readOnly, call("Write", map[string]any{"file_path": filepath.Join(out, "result.json"), "content": "{}"}, ro), ""},
		{"write a read-only checkout", readOnly, call("Write", map[string]any{"file_path": filepath.Join(ro, "README.md")}, ro), "may write only under"},
		{"edit the writable checkout", readWrite, call("Edit", map[string]any{"file_path": "docs/a.md"}, repo), ""},
		{"edit a checkout that is read-only here", readOnly, call("Edit", map[string]any{"file_path": "docs/a.md"}, repo), "is outside"},
		{"multi-edit through ..", readWrite, call("MultiEdit", map[string]any{"file_path": filepath.Join(repo, "..", "ro", "x")}, repo), "may write only under"},
		{"through a symlink", readWrite, call("Write", map[string]any{"file_path": filepath.Join(out, "escape", "x")}, repo), "may write only under"},
		{"a notebook", readWrite, call("NotebookEdit", map[string]any{"notebook_path": filepath.Join(repo, "n.ipynb")}, repo), ""},
		{"a write without a path", readWrite, call("Write", map[string]any{}, repo), "no file path"},
		{"a relative write without a cwd", readWrite, call("Write", map[string]any{"file_path": "x"}, ""), "relative path"},
		{"reads aren't checked", readOnly, call("Read", map[string]any{"file_path": "/etc/hosts"}, ro), ""},
		{"a plain command", readOnly, call("Bash", map[string]any{"command": "ls -la && git status"}, ro), ""},
		{"git push", readWrite, call("Bash", map[string]any{"command": "git push origin HEAD"}, repo), "factory push"},
		{"git push with options", readWrite, call("Bash", map[string]any{"command": "cd repo && git -C . --no-pager push -f"}, repo), "factory push"},
		{"git push in a subshell", readWrite, call("Bash", map[string]any{"command": `bash -c "/usr/bin/git push"`}, repo), "factory push"},
		{"factory push", readWrite, call("Bash", map[string]any{"command": "factory push --branch fx/x"}, repo), ""},
		{"gh pr view", readOnly, call("Bash", map[string]any{"command": "gh pr view 13 --json files"}, ro), ""},
		{"gh pr create", readWrite, call("Bash", map[string]any{"command": "gh pr create --fill"}, repo), "`gh pr create` is not allowed"},
		{"gh issue comment", readOnly, call("Bash", map[string]any{"command": "echo hi | gh issue comment 7 -F -"}, ro), "`gh issue comment` is not allowed"},
		{"gh api read", readOnly, call("Bash", map[string]any{"command": "gh api repos/acme/docs/pulls/13/files --paginate"}, ro), ""},
		{"gh api read with fields and GET", readOnly, call("Bash", map[string]any{"command": "gh api -X GET search/issues -f q=repo:acme/docs"}, ro), ""},
		{"gh api POST", readOnly, call("Bash", map[string]any{"command": "gh api --method POST repos/acme/docs/issues/7/comments -f body=x"}, ro), "gh api"},
		{"gh api fields", readOnly, call("Bash", map[string]any{"command": "gh api repos/acme/docs/issues/7/labels -f labels[]=bug"}, ro), "gh api"},
		{"gh api graphql query", readOnly, call("Bash", map[string]any{"command": `gh api graphql -f query='query { viewer { login } }'`}, ro), ""},
		{"gh api graphql mutation", readOnly, call("Bash", map[string]any{"command": `gh api graphql -f query='mutation { addComment(input: {}) { clientMutationId } }'`}, ro), "gh api"},
		{"unreadable input", readOnly, "not json", "could not be checked"},
		{"a bad tool input", readOnly, `{"tool_name":"Write","tool_input":"x"}`, "could not be checked"},
		{"no event", nil, call("Read", nil, ro), "usage"},
		{"a relative root", []string{"pre-tool-use", "--write", "out"}, call("Read", nil, ro), "bad arguments"},
		{"an extra argument", []string{"pre-tool-use", "extra"}, call("Read", nil, ro), "bad arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := HookMain(tt.args, strings.NewReader(tt.input), &stderr)
			switch {
			case tt.deny == "" && code != 0:
				t.Errorf("blocked (%d): %s", code, stderr.String())
			case tt.deny != "" && (code != hookExitBlock || !strings.Contains(stderr.String(), tt.deny)):
				t.Errorf("HookMain = %d %q, want %d with %q", code, stderr.String(), hookExitBlock, tt.deny)
			}
		})
	}
}

func TestHookInputLimit(t *testing.T) {
	var stderr bytes.Buffer
	big := strings.NewReader(`{"tool_name":"Write","tool_input":{"content":"` + strings.Repeat("x", maxHookInput) + `"}}`)
	if code := HookMain([]string{"pre-tool-use", "--write", t.TempDir()}, big, &stderr); code != hookExitBlock {
		t.Errorf("an oversized call = %d", code)
	}
}
