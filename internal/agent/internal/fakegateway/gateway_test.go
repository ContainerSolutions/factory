package fakegateway

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/runbundle"
)

// The tokens the gateway holds in these tests: none may appear in a Record.
const (
	rwToken    = "ghs_rwTaskTokenOnlyInTheGateway0123456789"
	roToken    = "ghs_roReadTokenOnlyInTheGateway0123456789"
	apiToken   = "ghs_apiTaskTokenOnlyInTheGateway012345678"
	agentToken = "agt_leaked-host.AgentTokenThatMustBeReplaced0123456789abcd"
)

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// echo is an upstream that answers with the Authorization it received, and counts its hits.
type echo struct {
	*httptest.Server
	hits atomic.Int64
}

func newEcho(t *testing.T) *echo {
	e := &echo{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Echo-Path", r.URL.RequestURI())
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(e.Close)
	return e
}

func config(disp, git, api *echo) Config {
	return Config{RunToken: RunToken, Dispatcher: disp.URL, GitUpstream: git.URL, APIUpstream: api.URL, APIToken: apiToken,
		Repos: map[string]Repo{"acme/docs": {Token: rwToken, RW: true}, "acme/ro": {Token: roToken}}}
}

// assertNoTokens fails when a record holds any token, or anything shaped like one.
func assertNoTokens(t *testing.T, recs []Record) {
	t.Helper()
	dump := fmt.Sprintf("%+v", recs)
	for _, tok := range []string{RunToken, rwToken, roToken, apiToken, agentToken, DefaultPlaceholder,
		basic(rwToken)[6:], basic(roToken)[6:], "x-access-token"} {
		if strings.Contains(dump, tok) {
			t.Errorf("records hold %.10q…: %s", tok, dump)
		}
	}
	if runbundle.LooksLikeToken(dump) {
		t.Errorf("records hold a token-shaped value: %s", dump)
	}
}

func TestRunTokenShape(t *testing.T) {
	if len(RunToken) != 47 || !runbundle.LooksLikeToken(RunToken) {
		t.Fatalf("RunToken %q isn't in the API's rtk_ shape", RunToken)
	}
}

func TestRoutes(t *testing.T) {
	const disp = "http://dispatcher.factory.internal"
	for _, c := range []struct {
		name, method, url, auth string
		wantStatus              int
		wantUpstream            string // the Authorization the upstream got; "-" when it isn't reached
		wantBody                string
		wantInjected            string
		wantClientAuth          string
	}{
		{"dispatcher injects the run token", "POST", disp + "/api/v1/tasks/task_1/effects", "", 200, "Bearer " + RunToken, "ok", InjectedRunToken, ClientAuthNone},
		{"dispatcher replaces a client bearer", "GET", disp + "/api/v1/tasks/task_1/prompt", "Bearer " + agentToken, 200, "Bearer " + RunToken, "ok", InjectedRunToken, "bearer other"},
		{"git fetch of the rw repo", "GET", "https://github.com/acme/docs.git/info/refs?service=git-upload-pack", "", 200, basic(rwToken), "ok", "git:acme/docs", ClientAuthNone},
		{"git fetch of a read repo, without .git", "POST", "https://github.com/acme/ro/git-upload-pack", "", 200, basic(roToken), "ok", "git:acme/ro", ClientAuthNone},
		{"git repo names are case-insensitive", "GET", "https://github.com/Acme/Docs.git/info/refs?service=git-upload-pack", "", 200, basic(rwToken), "ok", "git:acme/docs", ClientAuthNone},
		{"git fetch of an unknown repo is anonymous", "GET", "https://github.com/acme/public.git/info/refs?service=git-upload-pack", "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+DefaultPlaceholder)), 200, "", "ok", InjectedNone, "basic placeholder"},
		{"git replaces a client basic credential", "GET", "https://github.com/acme/docs.git/info/refs?service=git-upload-pack", basic(roToken), 200, basic(rwToken), "ok", "git:acme/docs", "basic other"},
		{"push to the rw repo: advertisement", "GET", "https://github.com/acme/docs.git/info/refs?service=git-receive-pack", "", 200, basic(rwToken), "ok", "git:acme/docs", ClientAuthNone},
		{"push to the rw repo: pack", "POST", "https://github.com/acme/docs.git/git-receive-pack", "", 200, basic(rwToken), "ok", "git:acme/docs", ClientAuthNone},
		{"push to a read repo: advertisement", "GET", "https://github.com/acme/ro.git/info/refs?service=git-receive-pack", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"push to a read repo: pack", "POST", "https://github.com/acme/ro.git/git-receive-pack", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"push to an unknown repo", "POST", "https://github.com/acme/public/git-receive-pack", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a dot-dot path is refused", "POST", "https://github.com/acme/docs/../ro.git/git-receive-pack", "", 400, "-", BadPath, InjectedNone, ClientAuthNone},
		{"an encoded slash is refused", "POST", "https://github.com/acme%2Fro.git/git-receive-pack", "", 400, "-", BadPath, InjectedNone, ClientAuthNone},
		{"api GET injects the token", "GET", "https://api.github.com/repos/acme/docs/pulls", "token " + DefaultPlaceholder, 200, "Bearer " + apiToken, "ok", InjectedAPI, "token placeholder"},
		{"api HEAD injects the token", "HEAD", "https://api.github.com/repos/acme/docs", "", 200, "Bearer " + apiToken, "", InjectedAPI, ClientAuthNone},
		{"api POST is refused", "POST", "https://api.github.com/repos/acme/docs/issues/1/comments", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api PATCH is refused", "PATCH", "https://api.github.com/repos/acme/docs/pulls/1", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api PUT is refused", "PUT", "https://api.github.com/repos/acme/docs/pulls/1/merge", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api DELETE is refused", "DELETE", "https://api.github.com/repos/acme/docs/git/refs/heads/x", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"plain HTTP to github.com is refused", "GET", "http://github.com/acme/docs.git/info/refs", "", 403, "-", PlainRefused, InjectedNone, ClientAuthNone},
		{"plain HTTP elsewhere is refused without AllowTunnel", "GET", "http://example.invalid/", "", 403, "-", TunnelRefused, InjectedNone, ClientAuthNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, g, a := newEcho(t), newEcho(t), newEcho(t)
			gw := Start(t, config(d, g, a))
			req, err := http.NewRequestWithContext(t.Context(), c.method, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			resp, err := gw.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != c.wantStatus || strings.TrimSpace(string(body)) != c.wantBody {
				t.Errorf("got %d %q, want %d %q", resp.StatusCode, body, c.wantStatus, c.wantBody)
			}
			hits := d.hits.Load() + g.hits.Load() + a.hits.Load()
			if c.wantUpstream == "-" {
				if hits != 0 {
					t.Errorf("a refused request reached an upstream (%d hits)", hits)
				}
			} else if got := resp.Header.Get("X-Echo-Auth"); hits != 1 || got != c.wantUpstream {
				t.Errorf("upstream got Authorization %.16q… (%d hits), want %.16q…", got, hits, c.wantUpstream)
			}
			recs := gw.Records()
			last := recs[len(recs)-1]
			u, _ := url.Parse(c.url)
			want := Record{Method: c.method, Host: strings.ToLower(u.Hostname()), Path: u.Path, Injected: c.wantInjected, Status: c.wantStatus, ClientAuth: c.wantClientAuth}
			if last != want {
				t.Errorf("record = %+v, want %+v", last, want)
			}
			if u.Scheme == "https" && (len(recs) != 2 || recs[0] != (Record{Method: "CONNECT", Host: want.Host, Status: 200, ClientAuth: ClientAuthNone})) {
				t.Errorf("records = %+v, want the CONNECT then the request", recs)
			}
			assertNoTokens(t, recs)
		})
	}
}

func TestNotAProxyRequest(t *testing.T) {
	gw := Start(t, Config{})
	resp, err := http.Get(gw.URL() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestNoUpstream(t *testing.T) {
	gw := Start(t, Config{RunToken: RunToken})
	resp, err := gw.Client().Get("http://dispatcher.factory.internal/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if _, err := New(Config{Dispatcher: "dispatcher:8080"}); err == nil {
		t.Fatal("New accepted a relative upstream")
	}
}

func TestConfiguredHosts(t *testing.T) {
	g, a := newEcho(t), newEcho(t)
	gw := Start(t, Config{GitHost: "Git.Example.Test", GitUpstream: g.URL, APIHost: "API.Example.Test", APIUpstream: a.URL, APIToken: apiToken,
		Repos: map[string]Repo{"acme/docs": {Token: rwToken}}})
	for u, want := range map[string]string{
		"https://git.example.test/acme/docs.git/info/refs?service=git-upload-pack": basic(rwToken),
		"https://api.example.test/user":                                            "Bearer " + apiToken,
	} {
		resp, err := gw.Client().Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("X-Echo-Auth"); got != want {
			t.Errorf("%s: upstream got %.16q…, want %.16q…", u, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	g := &Gateway{placeholders: map[string]bool{DefaultPlaceholder: true, "other-placeholder": true}}
	for in, want := range map[string]string{
		"":                             ClientAuthNone,
		"Bearer " + DefaultPlaceholder: "bearer placeholder",
		"token other-placeholder":      "token placeholder",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+DefaultPlaceholder)): "basic placeholder",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+rwToken)):            "basic other",
		"Basic not-base64!":  "basic other",
		"Bearer " + RunToken: "bearer other",
		rwToken:              "unknown other", // no scheme: the value is never taken as one
		"ghs_x " + rwToken:   "unknown other",
		"Digest username=x":  "unknown other",
	} {
		if got := g.classify(in); got != want {
			t.Errorf("classify(%.12q…) = %q, want %q", in, got, want)
		}
	}
}

// fakeTB records Errorf; the gateway's assertions only call Helper and Errorf.
type fakeTB struct {
	testing.TB
	errs []string
}

func (f *fakeTB) Helper()                        {}
func (f *fakeTB) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }

func TestAssertNoClientCredential(t *testing.T) {
	for _, c := range []struct {
		name  string
		auths []string
		fails int
	}{
		{"absent and placeholders pass", []string{"", "Bearer " + DefaultPlaceholder, "token " + DefaultPlaceholder}, 0},
		{"a real credential fails", []string{"", "Bearer " + agentToken}, 1},
		{"a bare token fails", []string{rwToken, "token " + DefaultPlaceholder}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newEcho(t)
			gw := Start(t, Config{APIUpstream: a.URL, APIToken: apiToken})
			for _, auth := range c.auths {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/user", nil)
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				resp, err := gw.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			tb := &fakeTB{}
			gw.AssertNoClientCredential(tb)
			if len(tb.errs) != c.fails {
				t.Errorf("%d failures, want %d: %q", len(tb.errs), c.fails, tb.errs)
			}
			if strings.Contains(strings.Join(tb.errs, "\n"), agentToken) || strings.Contains(strings.Join(tb.errs, "\n"), rwToken) {
				t.Errorf("the failure prints the credential: %q", tb.errs)
			}
		})
	}
}

func TestTunnel(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnelled "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(ts.Close)
	client := func(gw *Gateway) *http.Client {
		u, _ := url.Parse(gw.URL())
		tr := ts.Client().Transport.(*http.Transport).Clone()
		tr.Proxy = http.ProxyURL(u)
		return &http.Client{Transport: tr}
	}
	host := strings.TrimPrefix(ts.URL, "https://")
	hostname, _, _ := strings.Cut(host, ":")

	t.Run("allowed: as it is, end to end TLS", func(t *testing.T) {
		gw := Start(t, Config{AllowTunnel: true})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
		req.Header.Set("Authorization", "Bearer kept")
		resp, err := client(gw).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "tunnelled Bearer kept" {
			t.Errorf("body %q", body)
		}
		if got := gw.Records(); len(got) != 1 || got[0] != (Record{Method: "CONNECT", Host: hostname, Status: 200, ClientAuth: ClientAuthNone}) {
			t.Errorf("records %+v", got)
		}
	})
	t.Run("allowed: plain HTTP forwarded as it is", func(t *testing.T) {
		up := newEcho(t)
		gw := Start(t, Config{AllowTunnel: true})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, up.URL+"/x", nil)
		req.Header.Set("Authorization", "Bearer kept")
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("X-Echo-Auth") != "Bearer kept" {
			t.Errorf("got %d with Authorization %q", resp.StatusCode, resp.Header.Get("X-Echo-Auth"))
		}
	})
	t.Run("allowed: a failed dial is a 502", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		addr := closed.Listener.Addr().String()
		closed.Close()
		gw := Start(t, Config{AllowTunnel: true})
		resp, err := client(gw).Get("https://" + addr)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the tunnel to a closed port worked")
		}
		if got := gw.Records(); len(got) != 1 || got[0].Status != http.StatusBadGateway {
			t.Errorf("records %+v", got)
		}
	})
	t.Run("refused", func(t *testing.T) {
		gw := Start(t, Config{})
		resp, err := client(gw).Get(ts.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the tunnel was allowed")
		}
		if got := gw.Records(); len(got) != 1 || got[0].Status != http.StatusForbidden {
			t.Errorf("records %+v", got)
		}
	})
}

func TestCA(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(ca.CertPEM())
	if b == nil || b.Type != "CERTIFICATE" {
		t.Fatalf("CertPEM isn't a PEM certificate: %q", ca.CertPEM())
	}
	if _, err := x509.ParseCertificate(b.Bytes); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"github.com", "127.0.0.1"} {
		l, err := ca.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := ca.Leaf(host)
		if again != l {
			t.Errorf("%s: a second leaf was minted", host)
		}
		leaf, _ := x509.ParseCertificate(l.Certificate[0])
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: ca.Pool()}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	other, _ := NewCA()
	l, _ := other.Leaf("github.com")
	leaf, _ := x509.ParseCertificate(l.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: ca.Pool()}); err == nil {
		t.Error("another instance's leaf verified")
	}
}

func TestRecordsIsACopy(t *testing.T) {
	gw := Start(t, Config{})
	resp, err := http.Get(gw.URL())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	recs := gw.Records()
	recs[0].Status = 0
	if gw.Records()[0].Status != http.StatusBadRequest {
		t.Fatal("Records shares its slice")
	}
	gw.Close()
	gw.Close() // idempotent
}

func TestConcurrent(t *testing.T) {
	d, g, a := newEcho(t), newEcho(t), newEcho(t)
	gw := Start(t, config(d, g, a))
	urls := []string{"http://dispatcher.factory.internal/api/v1/tasks/t/effects",
		"https://github.com/acme/docs.git/info/refs?service=git-upload-pack", "https://api.github.com/user",
		"https://github.com/acme/ro.git/git-receive-pack"}
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			c := gw.Client()
			for _, u := range urls {
				resp, err := c.Get(u)
				if err != nil {
					t.Errorf("%d %s: %v", i, u, err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			_ = gw.Records()
		})
	}
	wg.Wait()
	var requests, refused int
	for _, r := range gw.Records() {
		if r.Method != "CONNECT" {
			requests++
		}
		if r.Status == http.StatusForbidden {
			refused++
		}
	}
	if requests != n*len(urls) || refused != n {
		t.Errorf("%d requests and %d refusals recorded, want %d and %d", requests, refused, n*len(urls), n)
	}
	if hits := d.hits.Load() + g.hits.Load() + a.hits.Load(); hits != int64(n*3) {
		t.Errorf("%d upstream hits, want %d", hits, n*3)
	}
	assertNoTokens(t, gw.Records())
}

// TestGitThroughTheGateway clones and pushes with the real git client through the gateway, to
// git's http-backend: a push to the rw repo lands, one to a read repo is refused, and git
// never holds a credential.
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
	tokens := map[string]string{"acme/docs": rwToken, "acme/ro": roToken}
	for repo := range tokens {
		seed := t.TempDir()
		must(seed, "init", "--quiet", "-b", "main")
		must(seed, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "initial")
		bare := filepath.Join(root, repo+".git")
		must(seed, "clone", "--quiet", "--bare", seed, bare)
		must(bare, "config", "http.receivepack", "true")
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
		InheritEnv: []string{"PATH"}, Stderr: io.Discard}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, _ := splitRepo(r.URL.Path)
		if _, p, ok := r.BasicAuth(); !ok || p != tokens[repo] {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	gw := Start(t, Config{GitUpstream: upstream.URL, Repos: map[string]Repo{"acme/docs": {Token: rwToken, RW: true}, "acme/ro": {Token: roToken}}})
	if err := os.WriteFile(caFile, gw.CA().CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := []string{"HTTPS_PROXY=" + gw.URL(), "https_proxy=" + gw.URL(), "NO_PROXY=", "no_proxy="}

	push := func(repo string) (string, error) {
		work := t.TempDir()
		if out, err := run(work, proxy, "clone", "--quiet", "https://github.com/"+repo+".git", "c"); err != nil {
			t.Fatalf("clone %s: %v\n%s", repo, err, out)
		}
		c := filepath.Join(work, "c")
		must(c, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "change")
		return run(c, proxy, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	}
	if out, err := push("acme/docs"); err != nil {
		t.Fatalf("push to the rw repo: %v\n%s", err, out)
	}
	if n, _ := run(filepath.Join(root, "acme/docs.git"), nil, "rev-list", "--count", "main"); strings.TrimSpace(n) != "2" {
		t.Errorf("the rw repo has %q commits on main, want 2", n)
	}
	if out, err := push("acme/ro"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("push to the read repo: err %v, want a 403\n%s", err, out)
	}
	if n, _ := run(filepath.Join(root, "acme/ro.git"), nil, "rev-list", "--count", "main"); strings.TrimSpace(n) != "1" {
		t.Errorf("the read repo has %q commits on main, want 1", n)
	}
	var refused bool
	for _, r := range gw.Records() {
		if r.Path == "/acme/ro.git/info/refs" && r.Status == http.StatusForbidden && r.Injected == InjectedNone {
			refused = true
		}
		if r.Status == http.StatusUnauthorized {
			t.Errorf("the upstream asked for credentials: %+v", r)
		}
	}
	if !refused {
		t.Errorf("no refused receive-pack advertisement in %+v", gw.Records())
	}
	gw.AssertNoClientCredential(t)
	assertNoTokens(t, gw.Records())
}
