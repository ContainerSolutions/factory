// Package login is the supervisor's store of the owner's model logins (supervise.md, "The
// workspace runner"): the Claude token `factory-agent login claude` reads on stdin and the codex
// ChatGPT login `factory-agent login codex` imports from codex's auth.json. Each is a file under
// <statedir>/logins (0700), written 0600 through a temp file and a rename. The gateway reads them
// on every request, so a re-import applies to the next one; nothing here refreshes a token.
//
// No value here is ever logged or printed: Codex's String, Format, LogValue and MarshalJSON
// redact, and every error names the file or the field, never its content.
package login

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Dir is the store's directory under the state dir.
const Dir = "logins"

// The store's files.
const (
	ClaudeFile = "claude.json"
	CodexFile  = "codex.json"
)

// maxFile bounds a login file and an imported auth.json.
const maxFile = 1 << 20

// ErrNoLogin is a login that was never stored.
var ErrNoLogin = errors.New("no login")

// Store is the login store at Path (<statedir>/logins).
type Store struct{ Path string }

// ForStateDir is the store under stateDir.
func ForStateDir(stateDir string) Store { return Store{Path: filepath.Join(stateDir, Dir)} }

type claudeFile struct {
	OAuthToken string `json:"oauthToken"`
}

type codexFile struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	AccountID    string `json:"accountId"`
	UserID       string `json:"userId"`
	PlanType     string `json:"planType"`
}

// claudeTokenRE is the shape of `claude setup-token`'s long-lived OAuth token.
var claudeTokenRE = regexp.MustCompile(`sk-ant-oat[A-Za-z0-9_-]+`)

// ClaudeToken is the one OAuth token in text: what `claude setup-token` printed around it
// (prose, a URL, terminal escapes), as pasted. A token repeated is one; none, or two different
// ones, is an error that names neither.
func ClaudeToken(text string) (string, error) {
	var tok string
	for _, m := range claudeTokenRE.FindAllString(text, -1) {
		switch {
		case tok == "":
			tok = m
		case m != tok:
			return "", errors.New("login claude: the input holds more than one OAuth token; paste only setup-token's")
		}
	}
	if tok == "" {
		if strings.TrimSpace(text) == "" {
			return "", errors.New("login claude: the input is empty")
		}
		return "", errors.New("login claude: the input holds no OAuth token from claude setup-token")
	}
	return tok, nil
}

// SaveClaude stores token, the one ClaudeToken found in `claude setup-token`'s output: one
// line, surrounding white space trimmed.
func (s Store) SaveClaude(token string) error {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return errors.New("login claude: the token is empty")
	case strings.ContainsAny(token, " \t\r\n"):
		return errors.New("login claude: the token is not one line without spaces")
	}
	return s.write(ClaudeFile, claudeFile{OAuthToken: token})
}

// Claude is the stored Claude OAuth token; ErrNoLogin when there is none.
func (s Store) Claude() (string, error) {
	var f claudeFile
	if err := s.read(ClaudeFile, &f); err != nil {
		return "", err
	}
	if f.OAuthToken == "" {
		return "", fmt.Errorf("%s: no token: %w", ClaudeFile, ErrNoLogin)
	}
	return f.OAuthToken, nil
}

// Codex is the owner's codex ChatGPT login: the tokens and the account identifiers the run
// bundle carries (runbundle.CodexAccount). It never prints its values.
type Codex struct {
	AccessToken, RefreshToken   string
	AccountID, UserID, PlanType string
}

func (Codex) String() string { return "login.Codex{redacted}" }

// Format redacts every verb.
func (c Codex) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// GoString redacts.
func (c Codex) GoString() string { return c.String() }

// LogValue redacts.
func (c Codex) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON redacts.
func (c Codex) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// SaveCodex stores c; it needs the access token and the three identifiers.
func (s Store) SaveCodex(c Codex) error {
	if err := c.check(); err != nil {
		return fmt.Errorf("login codex: %w", err)
	}
	return s.write(CodexFile, codexFile(c))
}

// Codex is the stored codex login; ErrNoLogin when there is none.
func (s Store) Codex() (Codex, error) {
	var f codexFile
	if err := s.read(CodexFile, &f); err != nil {
		return Codex{}, err
	}
	c := Codex(f)
	if err := c.check(); err != nil {
		return Codex{}, fmt.Errorf("%s: %w: %w", CodexFile, err, ErrNoLogin)
	}
	return c, nil
}

func (c Codex) check() error {
	for _, f := range []struct{ name, v string }{
		{"the access token", c.AccessToken}, {"chatgpt_account_id", c.AccountID},
		{"chatgpt_user_id", c.UserID}, {"chatgpt_plan_type", c.PlanType},
	} {
		if f.v == "" {
			return fmt.Errorf("%s is missing", f.name)
		}
	}
	return nil
}

// codex's auth.json: the keys codex itself writes (harness/codex's flags.go has the VM's).
type authJSON struct {
	Tokens *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// authClaims is the id_token's claim with the account identifiers.
const authClaims = "https://api.openai.com/auth"

// ParseCodexAuth reads codex's auth.json (a ChatGPT login): the tokens, and the account
// identifiers from the id_token's claims (its signature isn't checked: the file is the owner's
// own). tokens.account_id, when set, must match the claim's.
func ParseCodexAuth(b []byte) (Codex, error) {
	if len(b) > maxFile {
		return Codex{}, fmt.Errorf("auth.json is over %d bytes", maxFile)
	}
	var a authJSON
	if err := json.Unmarshal(b, &a); err != nil {
		return Codex{}, errors.New("auth.json is not JSON")
	}
	if a.Tokens == nil || a.Tokens.AccessToken == "" || a.Tokens.IDToken == "" {
		return Codex{}, errors.New("auth.json has no ChatGPT login (tokens.access_token and tokens.id_token): log codex in with ChatGPT first")
	}
	parts := strings.Split(a.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return Codex{}, errors.New("auth.json: tokens.id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Codex{}, errors.New("auth.json: tokens.id_token's payload is not base64url")
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Codex{}, errors.New("auth.json: tokens.id_token's payload is not a JSON object")
	}
	var acct struct {
		AccountID string `json:"chatgpt_account_id"`
		UserID    string `json:"chatgpt_user_id"`
		PlanType  string `json:"chatgpt_plan_type"`
	}
	if raw, ok := claims[authClaims]; !ok || json.Unmarshal(raw, &acct) != nil {
		return Codex{}, fmt.Errorf("auth.json: tokens.id_token has no %q claims", authClaims)
	}
	if a.Tokens.AccountID != "" && a.Tokens.AccountID != acct.AccountID {
		return Codex{}, errors.New("auth.json: tokens.account_id differs from the id_token's chatgpt_account_id")
	}
	c := Codex{AccessToken: a.Tokens.AccessToken, RefreshToken: a.Tokens.RefreshToken,
		AccountID: acct.AccountID, UserID: acct.UserID, PlanType: acct.PlanType}
	if err := c.check(); err != nil {
		return Codex{}, fmt.Errorf("auth.json: %w", err)
	}
	return c, nil
}

// write stores v as name: the directory 0700, the file 0600 through a temp file and a rename.
func (s Store) write(name string, v any) error {
	if s.Path == "" {
		return errors.New("login: the store has no path")
	}
	if err := os.MkdirAll(s.Path, 0o700); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if err := os.Chmod(s.Path, 0o700); err != nil { // an existing one too
		return fmt.Errorf("login: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("login: %s: %w", name, err)
	}
	f, err := os.CreateTemp(s.Path, "."+name+".*")
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	tmp := f.Name()
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.Path, name))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("login: %s: %w", name, err)
	}
	return nil
}

// read decodes name into v; ErrNoLogin when the file doesn't exist. It refuses anything but a
// regular file.
func (s Store) read(name string, v any) error {
	p := filepath.Join(s.Path, name)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", name, ErrNoLogin)
	}
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("login: %s is not a regular file", name)
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return fmt.Errorf("login: %s: %w", name, err)
	}
	if len(b) > maxFile {
		return fmt.Errorf("login: %s is over %d bytes", name, maxFile)
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), v); err != nil {
		return fmt.Errorf("login: %s is not JSON", name)
	}
	return nil
}
