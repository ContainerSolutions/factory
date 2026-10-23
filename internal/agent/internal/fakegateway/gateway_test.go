package fakegateway

import (
	"bufio"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
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
	"time"

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

// echo is an upstream that answers with the Authorization, the request URI and the other
// credential carriers it received, and counts its hits.
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
		var creds []string
		for _, h := range []string{"Proxy-Authorization", "X-Api-Key", "Cookie"} {
			if r.Header.Get(h) != "" {
				creds = append(creds, h)
			}
		}
		w.Header().Set("X-Echo-Creds", strings.Join(creds, ","))
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
		{"dispatcher replaces a client bearer", "POST", disp + "/api/v1/tasks/task_1/effects?attempt=2", "Bearer " + agentToken, 200, "Bearer " + RunToken, "ok", InjectedRunToken, "bearer other"},
		{"dispatcher host with a trailing dot", "POST", "http://Dispatcher.Factory.Internal./api/v1/tasks/task_1/effects", "", 200, "Bearer " + RunToken, "ok", InjectedRunToken, ClientAuthNone},
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
		{"a read repo: a duplicate service parameter", "GET", "https://github.com/acme/ro.git/info/refs?service=git-upload-pack&service=git-receive-pack", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a read repo: an upper-case pack path", "POST", "https://github.com/acme/ro.git/GIT-RECEIVE-PACK", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a read repo: an upper-case service value", "GET", "https://github.com/acme/ro.git/info/refs?service=GIT-RECEIVE-PACK", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a read repo: info/refs without a service", "GET", "https://github.com/acme/ro.git/info/refs", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a read repo: another path", "GET", "https://github.com/acme/ro.git/HEAD", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"a read repo: GET upload-pack", "GET", "https://github.com/acme/ro.git/git-upload-pack", "", 403, "-", PushRefused, InjectedNone, ClientAuthNone},
		{"git host with a trailing dot", "GET", "https://GitHub.com./acme/docs.git/info/refs?service=git-upload-pack", "", 200, basic(rwToken), "ok", "git:acme/docs", ClientAuthNone},
		{"a dot-dot path is refused", "POST", "https://github.com/acme/docs/../ro.git/git-receive-pack", "", 400, "-", BadPath, InjectedNone, ClientAuthNone},
		{"an encoded slash is refused", "POST", "https://github.com/acme%2Fro.git/git-receive-pack", "", 400, "-", BadPath, InjectedNone, ClientAuthNone},
		{"api GET injects the token", "GET", "https://api.github.com/repos/acme/docs/pulls?state=open&per_page=5", "token " + DefaultPlaceholder, 200, "Bearer " + apiToken, "ok", InjectedAPI, "token placeholder"},
		{"api host with a trailing dot", "GET", "https://api.github.com./user", "", 200, "Bearer " + apiToken, "ok", InjectedAPI, ClientAuthNone},
		{"api HEAD injects the token", "HEAD", "https://api.github.com/repos/acme/docs", "", 200, "Bearer " + apiToken, "", InjectedAPI, ClientAuthNone},
		{"api POST is refused", "POST", "https://api.github.com/repos/acme/docs/issues/1/comments", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api PATCH is refused", "PATCH", "https://api.github.com/repos/acme/docs/pulls/1", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api PUT is refused", "PUT", "https://api.github.com/repos/acme/docs/pulls/1/merge", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"api DELETE is refused", "DELETE", "https://api.github.com/repos/acme/docs/git/refs/heads/x", "", 403, "-", APIWriteRefused, InjectedNone, ClientAuthNone},
		{"plain HTTP to github.com is refused", "GET", "http://GitHub.com/acme/docs.git/info/refs", "", 403, "-", PlainRefused, InjectedNone, ClientAuthNone},
		{"plain HTTP to github.com. is refused", "GET", "http://github.com./acme/docs.git/info/refs", "", 403, "-", PlainRefused, InjectedNone, ClientAuthNone},
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
			} else {
				if got := resp.Header.Get("X-Echo-Auth"); hits != 1 || got != c.wantUpstream {
					t.Errorf("upstream got Authorization %.16q… (%d hits), want %.16q…", got, hits, c.wantUpstream)
				}
				if got := resp.Header.Get("X-Echo-Path"); got != req.URL.RequestURI() {
					t.Errorf("upstream got %q, want %q", got, req.URL.RequestURI())
				}
			}
			recs := gw.Records()
			last := recs[len(recs)-1]
			u, _ := url.Parse(c.url)
			want := Record{Method: c.method, Host: strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), Path: u.Path, Injected: c.wantInjected, Status: c.wantStatus, ClientAuth: c.wantClientAuth}
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
	if got := gw.Records(); len(got) != 1 || got[0].Host != "127.0.0.1" {
		t.Errorf("records %+v, want the host 127.0.0.1 without the port", got)
	}
}

func TestNoUpstream(t *testing.T) {
	gw := Start(t, Config{RunToken: RunToken})
	resp, err := gw.Client().Post("http://dispatcher.factory.internal/api/v1/tasks/t/effects", "application/json", nil)
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
		"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+DefaultPlaceholder)):      "basic placeholder",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+rwToken)):                 "basic other",
		"Basic " + base64.StdEncoding.EncodeToString([]byte(":"+DefaultPlaceholder)):                    "basic placeholder",
		"Basic " + base64.StdEncoding.EncodeToString([]byte(DefaultPlaceholder+":"+DefaultPlaceholder)): "basic placeholder",
		"Basic " + base64.StdEncoding.EncodeToString([]byte(rwToken+":"+DefaultPlaceholder)):            "basic other", // a token as the username
		"Basic " + base64.StdEncoding.EncodeToString([]byte("someone:"+DefaultPlaceholder)):             "basic other",
		"Basic " + base64.StdEncoding.EncodeToString([]byte(DefaultPlaceholder)):                        "basic other", // no password
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
	const effects = "http://dispatcher.factory.internal/api/v1/tasks/t/effects"
	const api = "https://api.github.com/user"
	type req struct {
		method, url string
		header      http.Header
		proxyUser   *url.Userinfo // HTTPS_PROXY's userinfo: Proxy-Authorization on the CONNECT or the request
	}
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Add(kv[i], kv[i+1])
		}
		return out
	}
	get := func(u string, kv ...string) req { return req{method: http.MethodGet, url: u, header: h(kv...)} }
	for _, c := range []struct {
		name  string
		reqs  []req
		fails int
	}{
		{"absent and placeholders pass", []req{get(api), get(api, "Authorization", "Bearer "+DefaultPlaceholder), get(api, "Authorization", "token "+DefaultPlaceholder),
			get(api, "X-Api-Key", DefaultPlaceholder)}, 0},
		{"a real credential fails", []req{get(api), get(api, "Authorization", "Bearer "+agentToken)}, 1},
		{"a bare token fails", []req{get(api, "Authorization", rwToken), get(api, "Authorization", "token "+DefaultPlaceholder)}, 1},
		{"a second Authorization value fails", []req{get(api, "Authorization", "Bearer "+DefaultPlaceholder, "Authorization", "Bearer "+agentToken)}, 1},
		{"a token as the Basic username fails", []req{get(api, "Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(rwToken+":"+DefaultPlaceholder)))}, 1},
		{"X-Api-Key with a token fails", []req{get(api, "X-Api-Key", apiToken)}, 1},
		{"a Cookie fails", []req{get(api, "Cookie", "user_session="+rwToken)}, 1},
		{"a Cookie to the dispatcher fails", []req{{method: http.MethodPost, url: effects, header: h("Cookie", "s="+rwToken, "X-Api-Key", apiToken)}}, 1},
		{"Proxy-Authorization on the CONNECT fails", []req{{method: http.MethodGet, url: api, proxyUser: url.UserPassword("x", rwToken)}}, 1},
		{"Proxy-Authorization on plain HTTP fails", []req{{method: http.MethodPost, url: effects, proxyUser: url.UserPassword("x", rwToken)}}, 1},
		{"a placeholder Proxy-Authorization passes", []req{{method: http.MethodGet, url: api, proxyUser: url.UserPassword("x", DefaultPlaceholder)},
			{method: http.MethodPost, url: effects, proxyUser: url.UserPassword("x", DefaultPlaceholder)}}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, a := newEcho(t), newEcho(t)
			gw := Start(t, Config{RunToken: RunToken, Dispatcher: d.URL, APIUpstream: a.URL, APIToken: apiToken})
			for _, r := range c.reqs {
				client := gw.Client()
				if r.proxyUser != nil {
					pu, _ := url.Parse(gw.URL())
					pu.User = r.proxyUser
					tr := gw.Client().Transport.(*http.Transport).Clone()
					tr.Proxy = http.ProxyURL(pu)
					client = &http.Client{Transport: tr}
					t.Cleanup(tr.CloseIdleConnections)
				}
				req, _ := http.NewRequestWithContext(t.Context(), r.method, r.url, nil)
				for k, vs := range r.header {
					req.Header[k] = vs
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s %s: status %d", r.method, r.url, resp.StatusCode)
				}
				if got := resp.Header.Get("X-Echo-Creds"); got != "" {
					t.Errorf("%s %s: the upstream got the client's %s", r.method, r.url, got)
				}
			}
			tb := &fakeTB{}
			gw.AssertNoClientCredential(tb)
			if len(tb.errs) != c.fails {
				t.Errorf("%d failures, want %d: %q; records %+v", len(tb.errs), c.fails, tb.errs, gw.Records())
			}
			errs := strings.Join(tb.errs, "\n")
			if strings.Contains(errs, agentToken) || strings.Contains(errs, rwToken) || strings.Contains(errs, apiToken) {
				t.Errorf("the failure prints the credential: %q", tb.errs)
			}
			assertNoTokens(t, gw.Records())
		})
	}
	t.Run("absolute-form URL userinfo fails", func(t *testing.T) {
		d := newEcho(t)
		gw := Start(t, Config{RunToken: RunToken, Dispatcher: d.URL})
		status, header := rawRequest(t, gw, "POST http://x:"+rwToken+"@dispatcher.factory.internal/api/v1/tasks/t/effects HTTP/1.1\r\n"+
			"Host: dispatcher.factory.internal\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		if status != http.StatusOK || header.Get("X-Echo-Auth") != "Bearer "+RunToken {
			t.Errorf("got %d, upstream Authorization %.16q…", status, header.Get("X-Echo-Auth"))
		}
		tb := &fakeTB{}
		gw.AssertNoClientCredential(tb)
		if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "userinfo basic other") || strings.Contains(tb.errs[0], rwToken) {
			t.Errorf("failures %q, want one for the userinfo; records %+v", tb.errs, gw.Records())
		}
		assertNoTokens(t, gw.Records())
	})
}

// rawRequest writes raw to the gateway on a new connection and reads one response's status and
// header.
func rawRequest(t *testing.T, gw *Gateway, raw string) (int, http.Header) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(gw.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func TestDispatcherRoutes(t *testing.T) {
	const disp = "http://dispatcher.factory.internal"
	prompt := append(DefaultDispatcherRoutes(), Route{Method: http.MethodGet, Path: "/api/v1/tasks/{id}/prompt"})
	for _, c := range []struct {
		name       string
		taskID     string
		routes     []Route
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"default: effects for any task", "", nil, "POST", "/api/v1/tasks/task_1/effects", 200, "ok"},
		{"default: effects with a query", "", nil, "POST", "/api/v1/tasks/task_1/effects?x=1", 200, "ok"},
		{"default: GET effects is refused", "", nil, "GET", "/api/v1/tasks/task_1/effects", 403, DispatcherRefused},
		{"default: the prompt is refused", "", nil, "GET", "/api/v1/tasks/task_1/prompt", 403, DispatcherRefused},
		{"default: a longer path is refused", "", nil, "POST", "/api/v1/tasks/task_1/effects/x", 403, DispatcherRefused},
		{"default: another API path is refused", "", nil, "POST", "/api/v1/tasks", 403, DispatcherRefused},
		{"task id: the task's effects", "task_1", nil, "POST", "/api/v1/tasks/task_1/effects", 200, "ok"},
		{"task id: another task's effects are refused", "task_1", nil, "POST", "/api/v1/tasks/task_2/effects", 403, DispatcherRefused},
		{"opt-in: the extra route", "task_1", prompt, "GET", "/api/v1/tasks/task_1/prompt", 200, "ok"},
		{"opt-in: effects still forwarded", "task_1", prompt, "POST", "/api/v1/tasks/task_1/effects", 200, "ok"},
		{"opt-in: another task's prompt is refused", "task_1", prompt, "GET", "/api/v1/tasks/task_2/prompt", 403, DispatcherRefused},
		{"non-canonical: dot-dot", "task_1", nil, "POST", "/api/v1/tasks/task_2/../task_1/effects", 400, BadPath},
		{"non-canonical: a double slash", "", nil, "POST", "/api/v1/tasks//effects", 400, BadPath},
		{"non-canonical: an encoded slash", "", nil, "POST", "/api/v1/tasks/a%2Fb/effects", 400, BadPath},
		{"non-canonical: an encoded letter", "task_1", nil, "POST", "/api/v1/tasks/%74ask_1/effects", 400, BadPath},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newEcho(t)
			gw := Start(t, Config{RunToken: RunToken, Dispatcher: d.URL, TaskID: c.taskID, DispatcherRoutes: c.routes})
			req, err := http.NewRequestWithContext(t.Context(), c.method, disp+c.path, nil)
			if err != nil {
				t.Fatal(err)
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
			wantHits, injected := int64(0), InjectedNone
			if c.wantStatus == http.StatusOK {
				wantHits, injected = 1, InjectedRunToken
				if got := resp.Header.Get("X-Echo-Path"); got != req.URL.RequestURI() {
					t.Errorf("upstream got %q, want %q", got, req.URL.RequestURI())
				}
			}
			if d.hits.Load() != wantHits {
				t.Errorf("%d upstream hits, want %d", d.hits.Load(), wantHits)
			}
			recs := gw.Records()
			want := Record{Method: c.method, Host: "dispatcher.factory.internal", Path: req.URL.Path, Injected: injected, Status: c.wantStatus, ClientAuth: ClientAuthNone}
			if len(recs) != 1 || recs[0] != want {
				t.Errorf("records %+v, want %+v", recs, want)
			}
		})
	}
}

func TestNewRejects(t *testing.T) {
	for name, cfg := range map[string]Config{
		"a repo without an owner":              {Repos: map[string]Repo{"docs": {}}},
		"a repo with an empty owner":           {Repos: map[string]Repo{"/docs": {}}},
		"a repo with an empty name":            {Repos: map[string]Repo{"acme/": {}}},
		"a repo that is only .git":             {Repos: map[string]Repo{"acme/.git": {}}},
		"a repo with three segments":           {Repos: map[string]Repo{"acme/docs/x": {}}},
		"repos that differ in case":            {Repos: map[string]Repo{"Acme/Docs": {RW: true}, "acme/docs": {}}},
		"repos that differ in .git":            {Repos: map[string]Repo{"acme/docs.git": {RW: true}, "acme/docs": {}}},
		"a route without a method":             {DispatcherRoutes: []Route{{Path: "/x"}}},
		"a route with a relative path":         {DispatcherRoutes: []Route{{Method: "GET", Path: "x"}}},
		"a route that isn't canonical":         {DispatcherRoutes: []Route{{Method: "GET", Path: "/a/../b"}}},
		"a route with an unknown pattern":      {DispatcherRoutes: []Route{{Method: "GET", Path: "/a/{name}"}}},
		"a route with a partial pattern":       {DispatcherRoutes: []Route{{Method: "GET", Path: "/a/x{id}"}}},
		"an upstream that isn't absolute http": {Dispatcher: "dispatcher:8080"},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := New(cfg)
			if err == nil {
				g.Close()
				t.Fatal("New accepted it")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("error %v isn't ErrConfig", err)
			}
		})
	}
}

func TestRepoKeysNormalised(t *testing.T) {
	g := newEcho(t)
	gw := Start(t, Config{GitUpstream: g.URL, Repos: map[string]Repo{"Acme/Docs.git": {Token: rwToken, RW: true}}})
	resp, err := gw.Client().Get("https://github.com/acme/docs/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Echo-Auth") != basic(rwToken) {
		t.Errorf("got %d with Authorization %.16q…, want the rw repo's", resp.StatusCode, resp.Header.Get("X-Echo-Auth"))
	}
}

// TestCloseClosesPendingIntercepts: Close closes an intercepted CONNECT that is still waiting
// for its TLS handshake, instead of leaving it to the handshake's timeout.
func TestCloseClosesPendingIntercepts(t *testing.T) {
	gw, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", strings.TrimPrefix(gw.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %d", resp.StatusCode)
	}
	start := time.Now()
	gw.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = br.ReadByte()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the intercepted connection is still open after Close")
	}
	if err == nil {
		t.Fatal("read a byte from a closed gateway")
	}
}

// TestCloseWaitsForHandlers: a request in flight when Close is called is recorded before Close
// returns, and nothing is recorded after.
func TestCloseWaitsForHandlers(t *testing.T) {
	hit := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(hit)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(up.Close)
	gw, err := New(Config{APIUpstream: up.URL, APIToken: apiToken})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := gw.Client().Get("https://api.github.com/user"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-hit
	gw.Close()
	recs := gw.Records()
	if len(recs) != 2 || recs[1].Method != http.MethodGet {
		t.Errorf("records after Close %+v, want the CONNECT and the GET", recs)
	}
	<-done
	if again := gw.Records(); len(again) != len(recs) {
		t.Errorf("recorded after Close: %+v", again[len(recs):])
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
	reqs := []struct{ method, url string }{{"POST", "http://dispatcher.factory.internal/api/v1/tasks/t/effects?n=1"},
		{"GET", "https://github.com/acme/docs.git/info/refs?service=git-upload-pack"}, {"GET", "https://api.github.com/user?page=2"},
		{"POST", "https://github.com/acme/ro.git/git-receive-pack"}}
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			c := gw.Client()
			for _, u := range reqs {
				req, _ := http.NewRequestWithContext(t.Context(), u.method, u.url, nil)
				resp, err := c.Do(req)
				if err != nil {
					t.Errorf("%d %s: %v", i, u.url, err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Echo-Path") != req.URL.RequestURI() {
					t.Errorf("%d %s: upstream got %q", i, u.url, resp.Header.Get("X-Echo-Path"))
				}
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
	if requests != n*len(reqs) || refused != n {
		t.Errorf("%d requests and %d refusals recorded, want %d and %d", requests, refused, n*len(reqs), n)
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
