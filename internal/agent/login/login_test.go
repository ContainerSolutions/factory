package login

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	access  = "codex-access-token-never-printed"
	refresh = "codex-refresh-token-never-printed"
)

// idToken is an unsigned JWT with claims.
func idToken(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(b) + ".sig"
}

func authFile(id, accountID string) []byte {
	b, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil, "last_refresh": "2026-10-01T00:00:00Z",
		"tokens": map[string]any{"id_token": id, "access_token": access, "refresh_token": refresh, "account_id": accountID},
	})
	return b
}

var goodClaims = map[string]any{"email": "o@example.com", authClaims: map[string]any{
	"chatgpt_account_id": "acct_1", "chatgpt_user_id": "user_1", "chatgpt_plan_type": "pro",
}}

func TestParseCodexAuth(t *testing.T) {
	tests := []struct {
		name, err string
		in        []byte
	}{
		{"ok", "", authFile(idToken(goodClaims), "acct_1")},
		{"no account_id key", "", authFile(idToken(goodClaims), "")},
		{"not JSON", "not JSON", []byte("{")},
		{"api key login", "no ChatGPT login", []byte(`{"OPENAI_API_KEY":"sk-x"}`)},
		{"id_token not a JWT", "not a JWT", authFile("abc", "acct_1")},
		{"no claims", "claims", authFile(idToken(map[string]any{"email": "o@example.com"}), "acct_1")},
		{"account differs", "differs", authFile(idToken(goodClaims), "acct_2")},
		{"no plan", "chatgpt_plan_type", authFile(idToken(map[string]any{authClaims: map[string]any{"chatgpt_account_id": "acct_1", "chatgpt_user_id": "u"}}), "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseCodexAuth(tt.in)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				if strings.Contains(err.Error(), access) {
					t.Errorf("the error holds the token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Codex{AccessToken: access, RefreshToken: refresh, AccountID: "acct_1", UserID: "user_1", PlanType: "pro"}
			if c.AccessToken != want.AccessToken || c.RefreshToken != want.RefreshToken || c.AccountID != want.AccountID || c.UserID != want.UserID || c.PlanType != want.PlanType {
				t.Errorf("parsed a different login")
			}
		})
	}
}

func TestStore(t *testing.T) {
	s := ForStateDir(t.TempDir())
	if _, err := s.Claude(); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Claude before a login: %v", err)
	}
	if _, err := s.Codex(); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Codex before a login: %v", err)
	}
	for _, bad := range []string{"", "  \n", "two words"} {
		if err := s.SaveClaude(bad); err == nil {
			t.Errorf("SaveClaude(%q) accepted", bad)
		}
	}
	if err := s.SaveClaude("  sk-ant-oat01-token\n"); err != nil {
		t.Fatal(err)
	}
	if tok, err := s.Claude(); err != nil || tok != "sk-ant-oat01-token" {
		t.Errorf("Claude: %v", err)
	}
	c := Codex{AccessToken: access, RefreshToken: refresh, AccountID: "a", UserID: "u", PlanType: "p"}
	if err := s.SaveCodex(Codex{AccessToken: access}); err == nil {
		t.Error("SaveCodex without the account accepted")
	}
	if err := s.SaveCodex(c); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Codex(); err != nil || got.AccessToken != access || got.AccountID != "a" {
		t.Errorf("Codex: %v", err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the store dir: %v %v", fi.Mode(), err)
	}
	for _, f := range []string{ClaudeFile, CodexFile} {
		fi, err := os.Stat(filepath.Join(s.Path, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}
	// A symlinked file is refused.
	other := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(other, []byte(`{"oauthToken":"x"}`), 0o600)
	_ = os.Remove(filepath.Join(s.Path, ClaudeFile))
	_ = os.Symlink(other, filepath.Join(s.Path, ClaudeFile))
	if _, err := s.Claude(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symlinked login: %v", err)
	}
}

func TestCodexRedacts(t *testing.T) {
	c := Codex{AccessToken: access, RefreshToken: refresh, AccountID: "a"}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "login", c)
	b, _ := json.Marshal(c)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%+v %#v", c, c), sb.String(), string(b)} {
		if strings.Contains(s, access) || strings.Contains(s, refresh) {
			t.Errorf("printed a token: %s", s)
		}
	}
}

// TestParseCodexAuthProvenance: codex's tokens.oauth_provenance and last_refresh are kept; a
// half or unsafe provenance is refused.
func TestParseCodexAuthProvenance(t *testing.T) {
	file := func(prov map[string]any) []byte {
		tokens := map[string]any{"id_token": idToken(goodClaims), "access_token": access, "refresh_token": refresh}
		if prov != nil {
			tokens["oauth_provenance"] = prov
		}
		b, _ := json.Marshal(map[string]any{"last_refresh": "2026-10-01T00:00:00Z", "tokens": tokens})
		return b
	}
	tests := []struct {
		name, err        string
		prov             map[string]any
		issuer, clientID string
	}{
		{"none", "", nil, "", ""},
		{"both", "", map[string]any{"issuer": "https://auth.example.com", "client_id": "app_x"}, "https://auth.example.com", "app_x"},
		{"no client id", "both issuer and client_id", map[string]any{"issuer": "https://auth.example.com"}, "", ""},
		{"http", "absolute https URL", map[string]any{"issuer": "http://auth.example.com", "client_id": "app_x"}, "", ""},
		{"user info", "absolute https URL", map[string]any{"issuer": "https://u:p@auth.example.com", "client_id": "app_x"}, "", ""},
		{"query", "absolute https URL", map[string]any{"issuer": "https://auth.example.com/?a=b", "client_id": "app_x"}, "", ""},
		{"fragment", "absolute https URL", map[string]any{"issuer": "https://auth.example.com/#f", "client_id": "app_x"}, "", ""},
		{"relative", "absolute https URL", map[string]any{"issuer": "/oauth", "client_id": "app_x"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseCodexAuth(file(tt.prov))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Issuer != tt.issuer || c.ClientID != tt.clientID || !c.LastRefresh.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("issuer %q, client id %q, last refresh %v", c.Issuer, c.ClientID, c.LastRefresh)
			}
		})
	}
}

// TestChatGPTStore: the refresher's Store loads the codex login and saves the refreshed tokens
// into it, keeping the account and the provenance, 0600.
func TestChatGPTStore(t *testing.T) {
	s := ForStateDir(t.TempDir())
	cs := ChatGPT{Store: s}
	if _, err := cs.Load(context.Background()); !errors.Is(err, ErrNoLogin) {
		t.Errorf("Load before a login: %v", err)
	}
	c := Codex{AccessToken: access, RefreshToken: refresh, AccountID: "a", UserID: "u", PlanType: "p", Issuer: "https://auth.example.com", ClientID: "app_x"}
	if err := s.SaveCodex(c); err != nil {
		t.Fatal(err)
	}
	l, err := cs.Load(context.Background())
	if err != nil || l.AccessToken != access || l.RefreshToken != refresh || l.AccountID != "a" || l.Issuer != c.Issuer || l.ClientID != c.ClientID {
		t.Fatalf("Load: %v", err)
	}
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	l.AccessToken, l.RefreshToken, l.LastRefresh, l.AccountID = "new-access", "new-refresh", at, "ignored"
	if err := cs.Save(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	got, err := s.Codex()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Codex{AccessToken: "new-access", RefreshToken: "new-refresh", AccountID: "a", UserID: "u", PlanType: "p", Issuer: c.Issuer, ClientID: c.ClientID, LastRefresh: at}); got != want {
		t.Errorf("stored %+v", got.LastRefresh)
	}
	if fi, err := os.Stat(filepath.Join(s.Path, CodexFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v", CodexFile, err)
	}
}
