package gitcred

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func ask(t *testing.T, socket, op, req string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Helper(context.Background(), []string{op}, strings.NewReader(req), &out, socket); err != nil {
		t.Fatalf("Helper %s: %v", op, err)
	}
	return out.String()
}

func TestServer(t *testing.T) {
	var mu sync.Mutex
	token := "task-token"
	entries := func() []Entry {
		mu.Lock()
		defer mu.Unlock()
		return []Entry{
			{Protocol: "https", Host: "github.com", Repo: "acme/docs", Username: "x-access-token", Password: token},
			{Protocol: "https", Host: "github.com", Repo: "acme/private", Username: "x-access-token", Password: "read-token"},
			{Protocol: "https", Host: "github.com", Repo: "acme/empty", Username: "x-access-token"},
			{Protocol: "https", Host: "github.com", Repo: "acme/bad", Username: "x\nusername=evil", Password: "p"},
		}
	}
	s, err := Listen(t.TempDir(), entries)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	st, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory %v %v, want 0700", st, err)
	}

	tests := []struct{ req, want string }{
		{"protocol=https\nhost=github.com\npath=acme/docs.git\n\n", "username=x-access-token\npassword=task-token\n"},
		{"protocol=https\nhost=GitHub.com\npath=/Acme/Docs\n", "username=x-access-token\npassword=task-token\n"},
		{"protocol=https\nhost=github.com\npath=acme/private.git\n", "username=x-access-token\npassword=read-token\n"},
		{"protocol=https\nhost=github.com\npath=acme/public.git\n", ""},    // not a task repo: anonymous
		{"protocol=https\nhost=github.com\n", ""},                          // no path: never a guess
		{"protocol=http\nhost=github.com\npath=acme/docs.git\n", ""},       // another protocol
		{"protocol=https\nhost=evil.example\npath=acme/docs.git\n", ""},    // another host
		{"protocol=https\nhost=github.com\npath=acme/docs.git/../x\n", ""}, // not the repo
		{"protocol=https\nhost=github.com\npath=acme/empty.git\n", ""},     // no token
		{"protocol=https\nhost=github.com\npath=acme/bad.git\n", ""},       // can't be framed
	}
	for _, tt := range tests {
		if got := ask(t, s.Path(), "get", tt.req); got != tt.want {
			t.Errorf("get %q = %q, want %q", tt.req, got, tt.want)
		}
	}
	if s.Answered() != 3 {
		t.Errorf("answered %d", s.Answered())
	}

	// A refresh applies at the next request.
	mu.Lock()
	token = "refreshed-token"
	mu.Unlock()
	if got := ask(t, s.Path(), "get", "protocol=https\nhost=github.com\npath=acme/docs\n"); !strings.Contains(got, "refreshed-token") {
		t.Errorf("after the refresh: %q", got)
	}

	// store and erase never reach the socket and print nothing.
	for _, op := range []string{"store", "erase"} {
		if got := ask(t, s.Path(), op, "protocol=https\nhost=github.com\npath=acme/docs\nusername=u\npassword=p\n"); got != "" {
			t.Errorf("%s printed %q", op, got)
		}
	}
	// Without a socket the helper answers nothing.
	if got := ask(t, "", "get", "protocol=https\nhost=github.com\npath=acme/docs\n"); got != "" {
		t.Errorf("no socket: %q", got)
	}
	if err := Helper(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, s.Path()); err == nil {
		t.Error("no op accepted")
	}

	dir := filepath.Dir(s.Path())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket directory survived Close: %v", err)
	}
	if err := Helper(context.Background(), []string{"get"}, strings.NewReader("protocol=https\n"), &bytes.Buffer{}, s.Path()); err == nil {
		t.Error("a closed server still answered")
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

func TestHelperConfig(t *testing.T) {
	cfg := HelperConfig([]string{"/opt/it's here/factory-agent", "fx", "git-credential"})
	if len(cfg) != 3 || cfg[0] != [2]string{"credential.helper", ""} || cfg[2] != [2]string{"credential.useHttpPath", "true"} {
		t.Fatalf("config %q", cfg)
	}
	if want := `!'/opt/it'\''s here/factory-agent' 'fx' 'git-credential'`; cfg[1][1] != want {
		t.Errorf("helper %q, want %q", cfg[1][1], want)
	}
	env := ConfigEnv(cfg)
	want := []string{"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=" + cfg[1][1],
		"GIT_CONFIG_KEY_2=credential.useHttpPath", "GIT_CONFIG_VALUE_2=true"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("env\n%s", strings.Join(env, "\n"))
	}
	// The quoting survives sh.
	out, err := exec.Command("sh", "-c", "printf '%s|' "+ShellQuote("a'b c")+" "+ShellQuote("$HOME")).Output()
	if err != nil || string(out) != "a'b c|$HOME|" {
		t.Errorf("sh saw %q %v", out, err)
	}
}

func TestParseRequest(t *testing.T) {
	got := ParseRequest([]byte("protocol=https\nhost=h\nnoequals\npath=a=b\n\nafter=blank\n"))
	if len(got) != 3 || got["path"] != "a=b" || got["after"] != "" {
		t.Errorf("parsed %v", got)
	}
}
