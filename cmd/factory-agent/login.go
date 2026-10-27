package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/agent/host"
	"github.com/ContainerSolutions/factory/internal/agent/login"
)

// loginDeps are the login command's injectable dependencies; the zero value is production.
type loginDeps struct {
	// Home is the owner's home (the state dir and ~/.codex); default os.UserHomeDir.
	Home func() (string, error)
	// Getenv reads XDG_STATE_HOME for the state dir on linux (host.Env.StateDir); default
	// os.Getenv.
	Getenv func(string) string
	// Stdin is where `login claude` reads setup-token's output; default os.Stdin.
	Stdin io.Reader
	// IsTerminal says whether Stdin is a terminal, where `login claude` prompts for the paste
	// first; default stdinIsTerminal.
	IsTerminal func() bool
}

// stdinIsTerminal is true when os.Stdin is a character device: a terminal (or /dev/null, where
// the prompt is harmless), not a pipe or a file.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func loginCommand() command {
	return command{
		name:    "login",
		summary: "store the owner's claude or codex login for VM workspaces (supervise)",
		run: func(_ context.Context, args []string, stdout, stderr io.Writer) int {
			return runLogin(args, stdout, stderr, loginDeps{})
		},
	}
}

func loginUsage(w io.Writer) {
	fmt.Fprint(w, `usage: factory-agent login <claude|codex> [flags]   (supervise.md)

  claude                 reads claude setup-token's output on stdin and stores the one token in it.
                         Run claude setup-token on its own (it is interactive) and copy what it
                         prints, then (the token stays out of argv and the shell history):
                           macOS: pbpaste | factory-agent login claude
                           Linux: factory-agent login claude, paste, then Enter and Ctrl-D;
                                  or xclip -o -selection clipboard | factory-agent login claude,
                                  or wl-paste | factory-agent login claude, where installed
  codex [--auth-json p]  imports codex's ChatGPT login (default ~/.codex/auth.json). Log codex in
                         on this machine first: install the codex CLI, then codex login (with
                         ChatGPT). Don't copy another machine's auth.json: both would refresh the
                         same login, each spending the other's refresh token.

Each goes to <state-dir>/logins/{claude,codex}.json (0600, in a 0700 directory); the
workspace gateway adds it to the model hosts' requests, and refreshes the codex login there
(the file then holds the newer tokens). --state-dir defaults to the agent's.
`)
}

// maxStdinToken bounds what login claude reads on stdin (setup-token's output, pasted).
const maxStdinToken = 64 << 10

func runLogin(args []string, stdout, stderr io.Writer, d loginDeps) int {
	if len(args) == 0 {
		loginUsage(stderr)
		return 2
	}
	which, args := args[0], args[1:]
	switch which {
	case "help", "-h", "--help":
		loginUsage(stdout)
		return 0
	case "claude", "codex":
	default:
		fmt.Fprintf(stderr, "factory-agent login: unknown login %q\n\n", which)
		loginUsage(stderr)
		return 2
	}
	if d.Home == nil {
		d.Home = os.UserHomeDir
	}
	if d.Stdin == nil {
		d.Stdin = os.Stdin
	}
	if d.IsTerminal == nil {
		d.IsTerminal = stdinIsTerminal
	}
	fs := flag.NewFlagSet("login "+which, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", "", "the agent's state dir (default: the agent's per OS, ~/Library/Application Support/factory-agent on a Mac, $XDG_STATE_HOME/factory-agent or ~/.local/state/factory-agent on Linux)")
	authJSON := fs.String("auth-json", "", "codex: the auth.json to import (default ~/.codex/auth.json)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "factory-agent login %s: unexpected arguments %v\n", which, fs.Args())
		return 2
	}
	if which == "claude" && *authJSON != "" {
		fmt.Fprintln(stderr, "factory-agent login claude: --auth-json is codex's")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "factory-agent login %s: %v\n", which, err)
		return 1
	}
	home, err := d.Home()
	if err != nil {
		return fail(fmt.Errorf("the home directory: %w", err))
	}
	if *stateDir == "" {
		*stateDir = (&host.Env{Home: home, Getenv: d.Getenv}).StateDir()
	}
	store := login.ForStateDir(*stateDir)
	if which == "claude" {
		if d.IsTerminal() {
			fmt.Fprintln(stderr, "Paste `claude setup-token`'s output, then press Enter and Ctrl-D.")
		}
		b, err := io.ReadAll(io.LimitReader(d.Stdin, maxStdinToken+1))
		if err != nil {
			return fail(fmt.Errorf("stdin: %w", err))
		}
		if len(b) > maxStdinToken {
			return fail(fmt.Errorf("stdin is over %d bytes", maxStdinToken))
		}
		tok, err := login.ClaudeToken(string(b))
		if err != nil {
			return fail(err)
		}
		if err := store.SaveClaude(tok); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "stored the claude login in", filepath.Join(store.Path, login.ClaudeFile))
		return 0
	}
	path := *authJSON
	if path == "" {
		path = filepath.Join(home, ".codex", "auth.json")
	}
	b, err := readCapped(path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("no codex login at %s: log codex in on this machine first (install the codex CLI, "+
			"then `codex login` with ChatGPT), then run this again. Don't copy another machine's auth.json: both "+
			"machines' gateways would refresh the same login, and each refresh spends the other's refresh token", path))
	}
	if err != nil {
		return fail(err)
	}
	c, err := login.ParseCodexAuth(b)
	if err != nil {
		return fail(fmt.Errorf("%s: %w", path, err))
	}
	if err := store.SaveCodex(c); err != nil {
		return fail(err)
	}
	fmt.Fprintln(stdout, "stored the codex login in", filepath.Join(store.Path, login.CodexFile))
	return 0
}

// readCapped reads a file of at most max bytes.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	return b, nil
}
