package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/login"
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
		{name: "codex, no auth.json", args: []string{"codex"}, code: 1, err: "auth.json"},
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
				Home: func() (string, error) { return home, nil }, Stdin: strings.NewReader(tt.stdin),
			})
			if code != tt.code || !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("code %d, stderr %q; want %d with %q", code, stderr.String(), tt.code, tt.err)
			}
			if out := stdout.String() + stderr.String(); strings.Contains(out, "secret") || strings.Contains(out, "sk-ant") {
				t.Errorf("printed a token: %q", out)
			}
			if tt.check != nil {
				tt.check(t, login.ForStateDir((&host.Env{Home: home}).StateDir()))
			}
		})
	}
}

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
