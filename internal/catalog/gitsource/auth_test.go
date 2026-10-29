package gitsource

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ContainerSolutions/factory/internal/catalog"
)

const testToken = "ghs_s3cr3tT0ken"

// authServer is a smart-HTTPS git server (git http-backend through net/http/cgi) with a
// public repo at /o/public.git and a private one at /o/private.git that answers 401 unless
// the request carries Basic x-access-token:testToken.
type authServer struct {
	ts *httptest.Server

	mu     sync.Mutex
	authed int // private-repo requests with the right credential
	denied int // private-repo requests without it
	sent   int // public-repo requests that carried an Authorization header anyway
}

func newAuthServer(t *testing.T, src string) *authServer {
	t.Helper()
	backend := filepath.Join(gitIn(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	root := t.TempDir()
	for _, name := range []string{"public", "private"} {
		gitIn(t, ".", "clone", "--quiet", "--bare", src, filepath.Join(root, "o", name+".git"))
	}
	h := &cgi.Handler{
		Path:       backend,
		Env:        []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
		InheritEnv: []string{"PATH"},
		Stderr:     io.Discard,
	}
	s := &authServer{}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// /o/<name>[.git]/…: http-backend serves /o/private as /o/private.git.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
		isPrivate := len(parts) == 3 && strings.TrimSuffix(parts[1], ".git") == "private"
		s.mu.Lock()
		switch {
		case isPrivate && (!ok || user != "x-access-token" || pass != testToken):
			s.denied++
			s.mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		case isPrivate:
			s.authed++
		case r.Header.Get("Authorization") != "":
			s.sent++
		}
		s.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.ts.Close)
	// The test server's certificate is self-signed; no global config or credential helper
	// (a keychain) may answer the 401s.
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	return s
}

func TestHeadAuth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	repo, sha := testRepo(t)
	srv := newAuthServer(t, repo)
	private, public := srv.ts.URL+"/o/private", srv.ts.URL+"/o/public.git"
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:wrong-" + testToken))

	tests := []struct {
		name, url string
		auth      Auth
		want      string
		gitErr    bool
		secrets   []string // must not appear in the error
	}{
		{name: "private with the token", url: private, auth: Auth{Token: testToken}, want: sha},
		{name: "private with the token and an explicit username", url: private + ".git", auth: Auth{Username: "x-access-token", Token: testToken}, want: sha},
		{name: "public anonymously", url: public, want: sha},
		{name: "private anonymously", url: private, gitErr: true},
		{name: "private with a wrong token", url: private, auth: Auth{Token: "wrong-" + testToken}, gitErr: true, secrets: []string{"wrong-" + testToken, header}},
		{name: "a token on a file URL", url: "file://" + repo, auth: Auth{Token: testToken}, gitErr: true, secrets: []string{testToken}},
		{name: "a token on a URL with user info", url: strings.Replace(private, "https://", "https://x-access-token:other@", 1), auth: Auth{Token: testToken}, gitErr: true, secrets: []string{testToken, "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HeadAuth(ctx, tt.url, "main", tt.auth)
			if tt.gitErr {
				if !errors.Is(err, ErrGit) {
					t.Fatalf("HeadAuth = %q, %v; want an ErrGit", got, err)
				}
				for _, s := range tt.secrets {
					if strings.Contains(err.Error(), s) {
						t.Errorf("the error has a secret: %v", err)
					}
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("HeadAuth = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.authed == 0 || srv.denied == 0 {
		t.Errorf("private repo: %d authenticated and %d denied requests, want some of each", srv.authed, srv.denied)
	}
	if srv.sent != 0 {
		t.Errorf("%d anonymous requests to the public repo carried credentials", srv.sent)
	}
}

func TestLoadAuth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	repo, sha := testRepo(t)
	srv := newAuthServer(t, repo)
	private := srv.ts.URL + "/o/private"
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + testToken))

	tests := []struct {
		name, url, ref string
		auth           Auth
		gitErr         bool
		secrets        []string // must not appear in the error
	}{
		{name: "private with the token", url: private, ref: "main", auth: Auth{Token: testToken}},
		{name: "private anonymously", url: private, ref: "main", gitErr: true},
		{name: "private with a wrong token", url: private, ref: "main", auth: Auth{Token: "wrong-" + testToken}, gitErr: true, secrets: []string{"wrong-" + testToken}},
		// git names the missing ref in its error: the token in it is redacted.
		{name: "a ref that is the token", url: private, ref: testToken, auth: Auth{Token: testToken}, gitErr: true, secrets: []string{testToken, header}},
		{name: "a token on a file URL", url: "file://" + repo, ref: "main", auth: Auth{Token: testToken}, gitErr: true, secrets: []string{testToken}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, errs, err := LoadAuth(ctx, tt.url, tt.ref, "catalog", catalog.Options{}, tt.auth)
			if tt.gitErr {
				if !errors.Is(err, ErrGit) {
					t.Fatalf("LoadAuth = %v; want an ErrGit", err)
				}
				for _, s := range tt.secrets {
					if strings.Contains(err.Error(), s) {
						t.Errorf("the error has a secret: %v", err)
					}
				}
				return
			}
			if err != nil || c == nil || c.SHA != sha {
				t.Fatalf("LoadAuth = %v, %v, %v; want the catalog at %s", c, errs, err, sha)
			}
		})
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.authed == 0 || srv.denied == 0 {
		t.Errorf("private repo: %d authenticated and %d denied requests, want some of each", srv.authed, srv.denied)
	}
}

// TestAuthConfig: the token reaches git only as a header in its environment, scoped to the
// repository URL, after the lock-down config.
func TestAuthConfig(t *testing.T) {
	const url = "https://github.com/compliance-framework/ccf-skills"
	env := environ(Auth{Token: testToken}.config(url))
	want := []string{
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=gc.auto",
		"GIT_CONFIG_KEY_3=http." + url + ".extraHeader",
		"GIT_CONFIG_VALUE_3=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken)),
	}
	for _, w := range want {
		if !slices.Contains(env, w) {
			t.Errorf("environment %q lacks %q", env, w)
		}
	}
	if got := environ(Auth{}.config(url)); !slices.Equal(got, gitEnv) {
		t.Errorf("anonymous environment = %q, want gitEnv %q", got, gitEnv)
	}
}
