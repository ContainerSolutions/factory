package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/host"
	"github.com/ContainerSolutions/factory/internal/agent/login"
)

// codexAuth is a codex auth.json with a ChatGPT login (an unsigned id_token).
func codexAuth(t *testing.T) []byte {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{
		"chatgpt_account_id": "acct_1", "chatgpt_user_id": "user_1", "chatgpt_plan_type": "pro"}})
	id := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".x"
	b, _ := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": id, "access_token": "at-secret", "refresh_token": "rt-secret"}})
	return b
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		home  func(t *testing.T, home string) // the fake home's files
		code  int
		check func(t *testing.T, s login.Store)
		err   string
	}{
		{name: "claude", args: []string{"claude"}, stdin: "sk-ant-oat01-abc\n", check: func(t *testing.T, s login.Store) {
			if tok, err := s.Claude(); err != nil || tok != "sk-ant-oat01-abc" {
				t.Errorf("claude login: %v", err)
			}
		}},
		{name: "claude, empty stdin", args: []string{"claude"}, code: 1, err: "empty"},
		{name: "claude, setup-token's output pasted", args: []string{"claude"},
			stdin: "Welcome to Claude Code\n\nBrowser didn't open? Use the url below:\nhttps://claude.ai/oauth/authorize?code=true\n\n" +
				"\x1b[32m✓\x1b[39m Long-lived authentication token created successfully!\n\nYour OAuth token (valid for 1 year):\n\n\x1b[1msk-ant-oat01-abc_DEF-123\x1b[22m\n\n" +
				"Store this token securely. You won't be able to see it again.\n\nUse this token by setting: export CLAUDE_CODE_OAUTH_TOKEN=<token>\n",
			check: func(t *testing.T, s login.Store) {
				if tok, err := s.Claude(); err != nil || tok != "sk-ant-oat01-abc_DEF-123" {
					t.Errorf("claude login: %v", err)
				}
			}},
		{name: "claude, the token twice", args: []string{"claude"}, stdin: "sk-ant-oat01-abc\nagain: sk-ant-oat01-abc.\n", check: func(t *testing.T, s login.Store) {
			if tok, err := s.Claude(); err != nil || tok != "sk-ant-oat01-abc" {
				t.Errorf("claude login: %v", err)
			}
		}},
		{name: "claude, no token", args: []string{"claude"}, stdin: "Welcome to Claude Code\nsk-ant-api03-key\n", code: 1, err: "no OAuth token"},
		{name: "claude, two tokens", args: []string{"claude"}, stdin: "sk-ant-oat01-abc\nsk-ant-oat01-xyz\n", code: 1, err: "more than one"},
		{name: "claude with --auth-json", args: []string{"claude", "--auth-json", "x"}, code: 2, err: "codex's"},
		{name: "codex, the default auth.json", args: []string{"codex"}, home: func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), codexAuth(t), 0o600); err != nil {
				t.Fatal(err)
			}
		}, check: func(t *testing.T, s login.Store) {
			if c, err := s.Codex(); err != nil || c.AccessToken != "at-secret" || c.AccountID != "acct_1" {
				t.Errorf("codex login: %v", err)
			}
		}},
		{name: "codex, no auth.json", args: []string{"codex"}, code: 1, err: "auth.json: log codex in on this machine first"},
		{name: "unknown", args: []string{"gemini"}, code: 2, err: "unknown login"},
		{name: "none", args: nil, code: 2, err: "usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.home != nil {
				tt.home(t, home)
			}
			var stdout, stderr bytes.Buffer
			code := runLogin(tt.args, &stdout, &stderr, loginDeps{
				Home: func() (string, error) { return home, nil }, Getenv: noEnv, Stdin: strings.NewReader(tt.stdin),
				IsTerminal: func() bool { return false },
			})
			if code != tt.code || !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("code %d, stderr %q; want %d with %q", code, stderr.String(), tt.code, tt.err)
			}
			if out := stdout.String() + stderr.String(); strings.Contains(out, "secret") || strings.Contains(out, "sk-ant") {
				t.Errorf("printed a token: %q", out)
			}
			if tt.check != nil {
				tt.check(t, login.ForStateDir((&host.Env{Home: home, Getenv: noEnv}).StateDir()))
			}
		})
	}
}

// noEnv is an environment without XDG_STATE_HOME: the tests' state dirs stay under their homes.
func noEnv(string) string { return "" }

// --auth-json and --state-dir name other places.
func TestLoginCodexPaths(t *testing.T) {
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, codexAuth(t), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runLogin([]string{"codex", "--auth-json", auth, "--state-dir", state}, &stdout, &stderr, loginDeps{
		Home: func() (string, error) { return t.TempDir(), nil },
	})
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if _, err := login.ForStateDir(state).Codex(); err != nil {
		t.Errorf("codex login: %v", err)
	}
}

// login claude prompts for the paste only when stdin is a terminal (a pipe needs no prompt), and
// says where the login went without printing it.
func TestLoginClaudePrompt(t *testing.T) {
	const prompt = "Paste `claude setup-token`'s output, then press Enter and Ctrl-D.\n"
	for _, tt := range []struct {
		name     string
		terminal bool
		stderr   string
	}{
		{name: "a terminal", terminal: true, stderr: prompt},
		{name: "a pipe", terminal: false, stderr: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := t.TempDir()
			var stdout, stderr bytes.Buffer
			code := runLogin([]string{"claude", "--state-dir", state}, &stdout, &stderr, loginDeps{
				Home: func() (string, error) { return t.TempDir(), nil }, Getenv: noEnv,
				Stdin: strings.NewReader("sk-ant-oat01-abc\n"), IsTerminal: func() bool { return tt.terminal },
			})
			if code != 0 || stderr.String() != tt.stderr {
				t.Fatalf("code %d, stderr %q; want 0 with %q", code, stderr.String(), tt.stderr)
			}
			want := "stored the claude login in " + filepath.Join(state, login.Dir, login.ClaudeFile) + "\n"
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q", stdout.String(), want)
			}
		})
	}
}

// Without an auth.json, login codex names the path and says what to do, and why not to copy one.
func TestLoginCodexMissing(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runLogin([]string{"codex"}, &stdout, &stderr, loginDeps{
		Home: func() (string, error) { return home, nil }, Getenv: noEnv, IsTerminal: func() bool { return false },
	})
	want := "factory-agent login codex: no codex login at " + filepath.Join(home, ".codex", "auth.json") +
		": log codex in on this machine first (install the codex CLI, then `codex login` with ChatGPT), then run " +
		"this again. Don't copy another machine's auth.json: both machines' gateways would refresh the same " +
		"login, and each refresh spends the other's refresh token\n"
	if code != 1 || stderr.String() != want {
		t.Fatalf("code %d, stderr %q; want 1 with %q", code, stderr.String(), want)
	}
}

// The usage covers macOS's clipboard, Linux's paste and clipboard tools, and codex's login.
func TestLoginUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runLogin([]string{"help"}, &stdout, &stderr, loginDeps{}); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	for _, want := range []string{
		"macOS: pbpaste | factory-agent login claude",
		"Linux: factory-agent login claude, paste, then Enter and Ctrl-D",
		"xclip -o -selection clipboard | factory-agent login claude",
		"wl-paste | factory-agent login claude",
		"install the codex CLI, then codex login (with",
		"Don't copy another machine's auth.json",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, stdout.String())
		}
	}
}
