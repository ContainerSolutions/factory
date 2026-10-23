package claudecode

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// HookOp is the `factory` CLI op (`factory-agent fx claude-hook pre-tool-use --write <dir>…`)
// that claude runs as its PreToolUse hook; hookEventArg its one event.
const (
	HookOp       = "claude-hook"
	hookEventArg = "pre-tool-use"
)

// maxHookInput bounds the hook's stdin (a Write call carries the whole file).
const maxHookInput = 64 << 20

// hookInput is what claude writes to a PreToolUse hook's stdin.
// UNVERIFIED (owner to verify with make smoke-claude)
type hookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
}

// HookMain is the PreToolUse hook. It reads claude's call from stdin and exits 0 to allow
// it, or hookExitBlock with the reason on stderr (claude gives it to the model) to block it:
//   - the file tools (Write, Edit, MultiEdit, NotebookEdit) may write only under a --write
//     directory (the run's out/ and tmp/, and the checkouts it may write), symlinks resolved;
//   - Bash may not `git push` or write to GitHub with gh (`gh pr|issue <write>`, `gh api`
//     with a write method or fields, a GraphQL mutation): writes go through the factory CLI.
//
// It fails closed: unreadable input or arguments block the call. It is a second layer that
// guards against mistakes and injected instructions: what a Bash command writes or reads is
// bounded by the OS sandbox (sandbox.go) or, under harness.IsolationVM, by the VM and its
// gateway, and the token scopes stay the boundary.
func HookMain(args []string, stdin io.Reader, stderr io.Writer) int {
	deny := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "factory: "+format+"\n", a...)
		return hookExitBlock
	}
	if len(args) == 0 || args[0] != hookEventArg {
		return deny("usage: factory %s %s --write <dir>…", HookOp, hookEventArg)
	}
	fs := flag.NewFlagSet(HookOp, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var roots []string
	fs.Func("write", "a directory the file tools may write under (repeated)", func(s string) error {
		if !filepath.IsAbs(s) {
			return fmt.Errorf("%q is not absolute", s)
		}
		roots = append(roots, s)
		return nil
	})
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return deny("%s %s: bad arguments: %v", HookOp, hookEventArg, err)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil || len(b) > maxHookInput {
		return deny("the tool call could not be checked")
	}
	var in hookInput
	if err := json.Unmarshal(b, &in); err != nil {
		return deny("the tool call could not be checked")
	}
	if reason := checkToolCall(in, roots); reason != "" {
		return deny("%s", reason)
	}
	return 0
}

// checkToolCall is why the call is blocked ("" to allow it).
func checkToolCall(in hookInput, roots []string) string {
	var ti toolInput
	if len(in.ToolInput) > 0 {
		if err := json.Unmarshal(in.ToolInput, &ti); err != nil {
			return "the tool call could not be checked"
		}
	}
	switch in.ToolName {
	case toolWrite, toolEdit, toolMultiEdit, toolNotebookEdit:
		p := ti.FilePath
		if in.ToolName == toolNotebookEdit {
			p = ti.NotebookPath
		}
		if p == "" {
			return in.ToolName + " has no file path"
		}
		if !filepath.IsAbs(p) {
			if !filepath.IsAbs(in.Cwd) {
				return in.ToolName + ": a relative path without a working directory"
			}
			p = filepath.Join(in.Cwd, p)
		}
		if !within(p, roots) {
			return fmt.Sprintf("%s may write only under %s in this run (%s is outside: a read-only checkout, or not part of the task)",
				in.ToolName, strings.Join(roots, ", "), p)
		}
	case toolBash:
		return checkCommand(ti.Command)
	}
	return ""
}

// within reports whether p is one of roots or under one, with symlinks in both resolved as
// far as they exist (a link in out/ to the checkout doesn't let a write through). A root of
// / (IsolationVM's writable workspace) holds every path.
func within(p string, roots []string) bool {
	rp := resolve(p)
	return slices.ContainsFunc(roots, func(r string) bool {
		rr := resolve(r)
		prefix := rr
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		return rp == rr || strings.HasPrefix(rp, prefix)
	})
}

// resolve is p cleaned, with its longest existing prefix's symlinks resolved.
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// A command word starts the command line or follows a separator, a subshell, a quote or a
// path ("/usr/bin/git").
const wordStart = "(?:^|[\\s;&|()`'\"/])"

var (
	// git [global options] push
	gitPushRE = regexp.MustCompile(wordStart + `git(?:\s+(?:-[cC]\s+\S+|--?[A-Za-z][\w-]*(?:=\S+)?))*\s+push\b`)
	// gh pr|issue <a write subcommand>
	ghWriteRE = regexp.MustCompile(wordStart + `gh\s+(?:pr|issue)\s+(?:create|merge|close|reopen|comment|edit|review|ready|delete|lock|unlock|transfer|pin|unpin|develop)\b`)
	// gh api <the rest of that command>
	ghAPIRE       = regexp.MustCompile(wordStart + `gh\s+api\b([^;&|\n]*)`)
	ghMethodRE    = regexp.MustCompile(`(?:\s-X\s*|\s--method[\s=]+)['"]?([A-Za-z]+)`)
	ghFieldsRE    = regexp.MustCompile(`\s(?:-f|-F|--field|--raw-field|--input)\b`)
	ghGraphQLRE   = regexp.MustCompile(`^\s+graphql\b`)
	ghMutationRE  = regexp.MustCompile(`(?i)\bmutation\b`)
	readOnlyVerbs = []string{"GET", "HEAD"}
)

// checkCommand is why a Bash command is blocked ("" to allow it): writes to GitHub go through
// the factory CLI (the headless preamble says so; effector.md's `factory push`).
func checkCommand(cmd string) string {
	if gitPushRE.MatchString(cmd) {
		return "`git push` is not allowed in this run: push with `factory push` (every change on GitHub goes through the factory CLI)"
	}
	if gh := ghWriteRE.FindString(cmd); gh != "" {
		return fmt.Sprintf("`%s` is not allowed in this run: every change on GitHub goes through the factory CLI", strings.TrimLeft(gh, " \t;&|()`'\"/"))
	}
	for _, m := range ghAPIRE.FindAllStringSubmatch(cmd, -1) {
		rest := " " + m[1]
		write := false
		switch {
		case ghGraphQLRE.MatchString(rest):
			write = ghMutationRE.MatchString(rest)
		case ghMethodRE.MatchString(rest):
			write = !slices.Contains(readOnlyVerbs, strings.ToUpper(ghMethodRE.FindStringSubmatch(rest)[1]))
		default:
			write = ghFieldsRE.MatchString(rest) // fields make gh api POST
		}
		if write {
			return "writing to GitHub with `gh api` is not allowed in this run: every change on GitHub goes through the factory CLI"
		}
	}
	return ""
}
