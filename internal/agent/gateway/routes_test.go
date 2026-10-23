package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

// The tokens hold secrets[0], so assertNoSecrets checks no record or log line has one.
const (
	rwToken  = "rw-sk-live-secret-token"
	roToken  = "ro-sk-live-secret-token"
	newToken = "new-sk-live-secret-token"
	agentTok = "agent-sk-live-secret-token"
)

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// testCreds: acme/rw is the task repo (contents write), acme/ro and acme/ro2 are read repos;
// a credential for another host is never used.
func testCreds() *runner.Credentials {
	return runner.NewCredentials(
		apiclient.GitCredential{Host: "github.com", Repo: "Acme/RW", Token: rwToken, Permissions: map[string]string{"contents": "write"}},
		apiclient.GitCredential{Host: "github.com", Repo: "acme/ro", ReadRepos: []string{"acme/ro2"}, Token: roToken},
		apiclient.GitCredential{Host: "ghe.example.com", Repo: "acme/other", Token: "ghe-" + secrets[0]},
	)
}

// inTunnel sends raw inside an intercepted tunnel to host.
func (h *env) inTunnel(t *testing.T, host, raw string) (int, http.Header, string) {
	t.Helper()
	tc, err := h.dialTLS(t, host, host)
	if err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, tc, bufio.NewReader(tc), raw)
}

// TestGitHubRoutes has a row per row of gateway.md's github.com table: the status, the
// refusal, whether the upstream was reached and the Authorization it got.
func TestGitHubRoutes(t *testing.T) {
	creds := testCreds()
	e, got := startIntercept(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") },
		func(c *Config) { c.Git = creds })
	tests := []struct {
		name, method, uri string
		code              policy.Code
		auth, injected    string
	}{
		{"rw fetch", "GET", "/acme/rw.git/info/refs?service=git-upload-pack", "", basic(rwToken), "git:acme/rw"},
		{"rw push refs", "GET", "/ACME/RW.git/info/refs?service=git-receive-pack", "", basic(rwToken), "git:acme/rw"},
		{"rw push", "POST", "/acme/rw.git/git-receive-pack", "", basic(rwToken), "git:acme/rw"},
		{"read fetch", "POST", "/acme/ro.git/git-upload-pack", "", basic(roToken), "git:acme/ro"},
		{"read repo's readRepos", "GET", "/acme/ro2/info/refs?service=git-upload-pack", "", basic(roToken), "git:acme/ro2"},
		{"read push refs", "GET", "/acme/ro.git/info/refs?service=git-receive-pack", policy.CodePushRefused, "", ""},
		{"read push", "POST", "/acme/ro.git/git-receive-pack", policy.CodePushRefused, "", ""},
		{"read objects", "GET", "/acme/ro.git/objects/info/packs", policy.CodePushRefused, "", ""},
		{"unknown fetch", "GET", "/acme/public.git/info/refs?service=git-upload-pack", "", "", ""},
		{"another host's repo", "GET", "/acme/other.git/info/refs?service=git-upload-pack", "", "", ""},
		{"unknown push", "POST", "/acme/public.git/git-receive-pack", policy.CodePushRefused, "", ""},
		{"web GET", "GET", "/acme/rw/releases/download/v1/x.tgz", "", "", ""},
		{"web POST", "POST", "/acme/rw/issues", policy.CodeWebWrite, "", ""},
		{"not canonical", "GET", "/acme/ro/../rw.git/info/refs?service=git-receive-pack", policy.CodeBadPath, "", ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(got.all())
			status, h, body := e.inTunnel(t, "github.com", tt.method+" "+tt.uri+" HTTP/1.1\r\nHost: github.com\r\n"+
				"Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x:header-secret"))+
				"\r\nCookie: session=header-secret\r\nContent-Length: 0\r\n\r\n")
			if tt.code != "" {
				want, _ := tt.code.Response("")
				if status != want.Status || policy.Code(h.Get(policy.RefusalHeader)) != tt.code || body != want.Body {
					t.Fatalf("got %d %q, want %s", status, body, tt.code)
				}
				if len(got.all()) != before {
					t.Fatal("a refused request reached the upstream")
				}
			} else {
				if status != http.StatusOK || body != "ok" {
					t.Fatalf("got %d %q", status, body)
				}
				r := got.all()[before]
				if a := r.Header.Get("Authorization"); a != tt.auth {
					t.Errorf("upstream got Authorization %q, want %q", a, tt.auth)
				}
				if c := r.Header.Get("Cookie"); c != "" {
					t.Errorf("upstream got Cookie %q", c)
				}
			}
			rec := e.sink.wait(t, 2*(i+1))[2*i+1]
			if rec.Mode != ModeIntercept || rec.Refusal != tt.code || rec.Injected != tt.injected {
				t.Errorf("record %+v, want %s injected %q", rec, tt.code, tt.injected)
			}
		})
	}

	t.Run("a heartbeat's refresh applies to the next request", func(t *testing.T) {
		c := creds.Get()
		c.Token = newToken
		creds.Set(c)
		creds.SetReads(nil)
		before := len(got.all())
		if status, _, _ := e.inTunnel(t, "github.com", "GET /acme/rw.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\n\r\n"); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if a := got.all()[before].Header.Get("Authorization"); a != basic(newToken) {
			t.Errorf("upstream got %q, want the refreshed token", a)
		}
		// acme/ro has no credential now: unknown, so a fetch goes anonymously.
		if status, _, _ := e.inTunnel(t, "github.com", "GET /acme/ro.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\n\r\n"); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if a := got.all()[before+1].Header.Get("Authorization"); a != "" {
			t.Errorf("upstream got %q for a repo without a credential", a)
		}
	})
}

// TestGitHubAPIRoutes (HEAD is policy's TestDecideGitHubAPI's): reads get the task token as Bearer, writes are refused, and POST /graphql
// passes only when the document only queries, with its body intact.
func TestGitHubAPIRoutes(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	e, got := startIntercept(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}, func(c *Config) { c.Git = testCreds() })
	query := `{"query":"query { viewer { login } }"}`
	mutation := `{"query":"mutation { addComment(input: {}) { id } }"}`
	tests := []struct {
		name, raw string
		code      policy.Code
	}{
		{"GET", "GET /repos/acme/rw/branches/feature%2Fx HTTP/1.1\r\nHost: api.github.com\r\n\r\n", ""},
		{"graphql query", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json; charset=utf-8\r\n" +
			"Content-Length: " + strconv.Itoa(len(query)) + "\r\n\r\n" + query, ""},
		{"graphql query, chunked", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n" + strconv.FormatInt(int64(len(query)), 16) + "\r\n" + query + "\r\n0\r\n\r\n", ""},
		{"graphql mutation", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(len(mutation)) + "\r\n\r\n" + mutation, policy.CodeAPIWrite},
		{"graphql form-encoded", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/x-www-form-urlencoded\r\n" +
			"Content-Length: " + strconv.Itoa(len(query)) + "\r\n\r\n" + query, policy.CodeAPIWrite},
		{"graphql two content types", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\n" +
			"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: " + strconv.Itoa(len(query)) + "\r\n\r\n" + query, policy.CodeAPIWrite},
		{"graphql gzip", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\nContent-Encoding: gzip\r\n" +
			"Content-Length: " + strconv.Itoa(len(query)) + "\r\n\r\n" + query, policy.CodeAPIWrite},
		{"graphql too long", "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(policy.MaxGraphQLBody+1) + "\r\n\r\n", policy.CodeAPIWrite},
		{"POST", "POST /repos/acme/rw/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 2\r\n\r\n{}", policy.CodeAPIWrite},
		{"DELETE", "DELETE /repos/acme/rw HTTP/1.1\r\nHost: api.github.com\r\n\r\n", policy.CodeAPIWrite},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(got.all())
			status, h, _ := e.inTunnel(t, "api.github.com", tt.raw)
			rec := e.sink.wait(t, 2*(i+1))[2*i+1]
			if rec.Refusal != tt.code {
				t.Errorf("record %+v, want %q", rec, tt.code)
			}
			if tt.code != "" {
				if status != http.StatusForbidden || policy.Code(h.Get(policy.RefusalHeader)) != tt.code {
					t.Fatalf("got %d %s, want %s", status, h.Get(policy.RefusalHeader), tt.code)
				}
				if len(got.all()) != before {
					t.Fatal("a refused request reached the upstream")
				}
				return
			}
			if status != http.StatusOK {
				t.Fatalf("got %d %s", status, h.Get(policy.RefusalHeader))
			}
			if a := got.all()[before].Header.Get("Authorization"); a != "Bearer "+rwToken {
				t.Errorf("upstream got Authorization %q", a)
			}
			if rec.Injected != InjectedGitHubAPI {
				t.Errorf("record %+v, want injected github-api", rec)
			}
			if up := got.all()[before]; up.URL.Path == "/graphql" &&
				(!slices.Equal(up.Header.Values("Content-Type"), []string{"application/json"}) || up.Header.Get("Content-Encoding") != "") {
				t.Errorf("the upstream got Content-Type %q, Content-Encoding %q", up.Header.Values("Content-Type"), up.Header.Get("Content-Encoding"))
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(strings.Join(bodies, "|"), query); n != 2 {
		t.Errorf("the upstream got %q, want the query twice", bodies)
	}
}

// dispatcherServer records what the dispatcher got.
type dispatcherServer struct {
	*httptest.Server
	got    *seen
	bodies chan string
}

func newDispatcher(t *testing.T, tlsOn bool) *dispatcherServer {
	t.Helper()
	d := &dispatcherServer{got: &seen{}, bodies: make(chan string, 16)}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.got.add(r.Clone(context.Background()))
		d.bodies <- string(b)
		w.WriteHeader(http.StatusAccepted)
	})
	if tlsOn {
		d.Server = httptest.NewTLSServer(h)
	} else {
		d.Server = httptest.NewServer(h)
	}
	t.Cleanup(d.Close)
	return d
}

// TestDispatcherRoute: POST effects reaches a 127.0.0.1 dispatcher (which the egress dialer
// refuses) and an https one through UpstreamRoots, with its path joined to the base's, every
// carrier and Cookie stripped, and the dispatcher token; every other route is refused.
func TestDispatcherRoute(t *testing.T) {
	for _, tlsOn := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[tlsOn], func(t *testing.T) {
			d := newDispatcher(t, tlsOn)
			e := start(t, func(c *Config) {
				c.Dispatcher, _ = url.Parse(d.URL + "/base/")
				c.DispatcherToken = func() string { return agentTok }
				if tlsOn {
					c.UpstreamRoots = d.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
				}
			})
			rp := e.send(t, "POST http://dispatcher.factory.internal/api/v1/tasks/task-1/effects?a=1 HTTP/1.1\r\n"+
				"Host: dispatcher.factory.internal\r\nAuthorization: Bearer header-secret\r\nX-Api-Key: header-secret\r\n"+
				"Cookie: s=header-secret\r\nContent-Length: 2\r\n\r\n{}")
			if rp.status != http.StatusAccepted || rp.code != "" {
				t.Fatalf("got %d %s", rp.status, rp.code)
			}
			r := d.got.all()[0]
			if r.URL.Path != "/base/api/v1/tasks/task-1/effects" || r.URL.RawQuery != "a=1" {
				t.Errorf("the dispatcher got %s", r.URL)
			}
			if a := r.Header.Get("Authorization"); a != "Bearer "+agentTok {
				t.Errorf("the dispatcher got Authorization %q", a)
			}
			if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
				t.Errorf("the dispatcher got a carrier: %v", r.Header)
			}
			if b := <-d.bodies; b != "{}" {
				t.Errorf("the dispatcher got the body %q", b)
			}
			rec := last(t, e.sink.wait(t, 1))
			if rec.Mode != ModeDispatcher || rec.Status != http.StatusAccepted || rec.Injected != InjectedDispatcher ||
				rec.Path != "/api/v1/tasks/task-1/effects" || rec.Up != 2 {
				t.Errorf("record %+v", rec)
			}
		})
	}

	d := newDispatcher(t, false)
	e := start(t, func(c *Config) { c.Dispatcher, _ = url.Parse(d.URL) })
	refused := []struct {
		name, raw string
		code      policy.Code
		mode      Mode
	}{
		{"another task", "POST http://dispatcher.factory.internal/api/v1/tasks/task-2/effects HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", policy.CodeDispatcherRoute, ModeDispatcher},
		{"GET effects", "GET http://dispatcher.factory.internal/api/v1/tasks/task-1/effects HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeDispatcherRoute, ModeDispatcher},
		{"the prompt", "GET http://dispatcher.factory.internal/api/v1/tasks/task-1/prompt HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeDispatcherRoute, ModeDispatcher},
		{"the transcript", "PUT http://dispatcher.factory.internal/api/v1/runs/run-1/transcript HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", policy.CodeDispatcherRoute, ModeDispatcher},
		{"not canonical", "POST http://dispatcher.factory.internal/api/v1/tasks/x/../task-1/effects HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", policy.CodeBadPath, ModeDispatcher},
		{"another port", "POST http://dispatcher.factory.internal:8080/api/v1/tasks/task-1/effects HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", policy.CodeDispatcherRoute, ModeForward},
		{"CONNECT", "CONNECT dispatcher.factory.internal:443 HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodePlainOnly, ModeConnect},
		{"plain github.com", "GET http://github.com/acme/rw.git/info/refs HTTP/1.1\r\nHost: github.com\r\n\r\n", policy.CodePlainHTTP, ModeForward},
	}
	for i, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			rp := e.send(t, tt.raw)
			if want, _ := tt.code.Response(""); rp.status != want.Status || rp.code != tt.code {
				t.Fatalf("got %d %s, want %s", rp.status, rp.code, tt.code)
			}
			if r := e.sink.wait(t, i+1)[i]; r.Refusal != tt.code || r.Mode != tt.mode {
				t.Errorf("record %+v, want %s/%s", r, tt.mode, tt.code)
			}
		})
	}
	if n := len(d.got.all()); n != 0 {
		t.Errorf("a refused request reached the dispatcher %d times", n)
	}

	t.Run("the transport dials only the dispatcher", func(t *testing.T) {
		_, err := e.g.dispatcher.DialContext(context.Background(), "tcp", "127.0.0.1:9")
		if !errors.Is(err, errDispatcherAddr) {
			t.Errorf("dialled another address: %v", err)
		}
	})
}

// TestGitThroughTheGateway clones and pushes with the real git client through the gateway to
// git's http-backend behind a TLS upstream: a push to the rw repo lands, one to a read repo is
// refused at info/refs before the upstream sees it, and git never holds a credential.
func TestGitThroughTheGateway(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	root, home := t.TempDir(), t.TempDir()
	caFile := filepath.Join(home, "ca.pem")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0", "GIT_SSL_CAINFO=" + caFile, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	run := func(dir string, extra []string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), gitBin, args...)
		cmd.Dir, cmd.Env = dir, append(append([]string(nil), env...), extra...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if out, err := run(dir, nil, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	tokens := map[string]string{"acme/rw": rwToken, "acme/ro": roToken}
	for repo := range tokens {
		seed := t.TempDir()
		must(seed, "init", "--quiet", "-b", "main")
		must(seed, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "initial")
		bare := filepath.Join(root, repo+".git")
		must(seed, "clone", "--quiet", "--bare", seed, bare)
		must(bare, "config", "http.receivepack", "true")
	}
	cgiH := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	e, got := startIntercept(t, func(w http.ResponseWriter, r *http.Request) {
		repo := policy.ParseGitHubPath(r.URL.Path).Repo
		if _, p, ok := r.BasicAuth(); !ok || p != tokens[repo] {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		cgiH.ServeHTTP(w, r)
	}, func(c *Config) { c.Git = testCreds() })
	if err := os.WriteFile(caFile, e.g.cfg.CA.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := []string{"HTTPS_PROXY=http://" + e.addr, "https_proxy=http://" + e.addr, "NO_PROXY=", "no_proxy="}
	push := func(repo string) (string, error) {
		work := t.TempDir()
		if out, err := run(work, proxy, "clone", "--quiet", "https://github.com/"+repo+".git", "c"); err != nil {
			t.Fatalf("clone %s: %v\n%s", repo, err, out)
		}
		c := filepath.Join(work, "c")
		must(c, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "change")
		return run(c, proxy, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	}
	if out, err := push("acme/rw"); err != nil {
		t.Fatalf("push to the rw repo: %v\n%s", err, out)
	}
	if n, _ := run(filepath.Join(root, "acme/rw.git"), nil, "rev-list", "--count", "main"); strings.TrimSpace(n) != "2" {
		t.Errorf("the rw repo has %q commits on main, want 2", n)
	}
	if out, err := push("acme/ro"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("push to the read repo: err %v, want a 403\n%s", err, out)
	}
	if n, _ := run(filepath.Join(root, "acme/ro.git"), nil, "rev-list", "--count", "main"); strings.TrimSpace(n) != "1" {
		t.Errorf("the read repo has %q commits on main, want 1", n)
	}
	for _, r := range got.all() {
		if strings.HasPrefix(r.URL.Path, "/acme/ro") && r.URL.RawQuery == "service=git-receive-pack" {
			t.Errorf("the upstream saw the read repo's receive-pack: %s", r.URL)
		}
	}
	refused := false
	for _, r := range e.sink.all() {
		if r.Path == "/acme/ro.git/info/refs" && r.Refusal == policy.CodePushRefused {
			refused = true
		}
		if r.Status == http.StatusUnauthorized {
			t.Errorf("the upstream asked for credentials: %+v", r)
		}
	}
	if !refused {
		t.Errorf("no refused receive-pack advertisement in %+v", e.sink.all())
	}
}
