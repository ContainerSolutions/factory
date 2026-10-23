package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/agent/harness"
)

// The model credentials hold secrets[0], so assertNoSecrets checks no record or log line has
// one. The account id is an identifier, not a secret.
const (
	oauthTok  = "oauth-sk-live-secret-token"
	apiKey    = "key-sk-live-secret-token"
	accessTok = "access-sk-live-secret-token"
	accountID = "acct-1"
)

func anthropicFrom(a AnthropicAuth, err error) func(context.Context) (AnthropicAuth, error) {
	return func(context.Context) (AnthropicAuth, error) { return a, err }
}

func chatgptFrom(l ChatGPTLogin, err error) func(context.Context) (ChatGPTLogin, error) {
	return func(context.Context) (ChatGPTLogin, error) { return l, err }
}

// modelUpstream answers "ok", or 401 on /401.
func modelUpstream(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/401" {
		http.Error(w, "expired", http.StatusUnauthorized)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

// call sends one request in a tunnel to host and returns the answer and the request's record.
func (h *env) call(t *testing.T, host, raw string) (int, policy.Code, Record) {
	t.Helper()
	before := len(h.sink.all())
	status, hd, _ := h.inTunnel(t, host, raw)
	return status, policy.Code(hd.Get(policy.RefusalHeader)), h.sink.wait(t, before+2)[before+1]
}

func req(host, path string, headers ...string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\n" + strings.Join(headers, "") + "\r\n"
}

const (
	phBearer = "Authorization: Bearer " + harness.Placeholder + "\r\n"
	phKey    = "X-Api-Key: " + harness.Placeholder + "\r\n"
	realAuth = "Authorization: Bearer sk-live-secret-token\r\n"
)

// TestAnthropicRoutes: the presence rule in both modes on every Anthropic host. A request with
// either carrier gets the gateway's credential in the gateway's form, every carrier stripped;
// one without goes without; no credential with a carrier is no-login.
func TestAnthropicRoutes(t *testing.T) {
	type want struct {
		status        int
		code          policy.Code
		auth, key     string
		injected, cli string
	}
	tests := []struct {
		name    string
		auth    func(context.Context) (AnthropicAuth, error)
		headers []string
		want    want
	}{
		{"oauth, placeholder bearer", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok}, nil), []string{phBearer},
			want{200, "", "Bearer " + oauthTok, "", InjectedAnthropicOAuth, ClientPlaceholder}},
		{"oauth, placeholder key", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok}, nil), []string{phKey},
			want{200, "", "Bearer " + oauthTok, "", InjectedAnthropicOAuth, ClientPlaceholder}},
		{"oauth, an empty carrier", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok}, nil), []string{"X-Api-Key:\r\n"},
			want{200, "", "Bearer " + oauthTok, "", InjectedAnthropicOAuth, ClientNone}},
		{"key, placeholder bearer", anthropicFrom(AnthropicAuth{APIKey: apiKey}, nil), []string{phBearer},
			want{200, "", "", apiKey, InjectedAnthropicKey, ClientPlaceholder}},
		{"key, both carriers, one a credential", anthropicFrom(AnthropicAuth{APIKey: apiKey}, nil), []string{phKey, realAuth},
			want{200, "", "", apiKey, InjectedAnthropicKey, ClientOther}},
		{"no carrier", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok}, nil), nil,
			want{200, "", "", "", "", ClientNone}},
		{"a stale credential with an error", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok}, errors.New("store: stale")), []string{phBearer},
			want{200, "", "Bearer " + oauthTok, "", InjectedAnthropicOAuth, ClientPlaceholder}},
		{"no login", anthropicFrom(AnthropicAuth{}, errors.New("store: none")), []string{phBearer},
			want{503, policy.CodeNoLogin, "", "", "", ClientPlaceholder}},
		{"both fields", anthropicFrom(AnthropicAuth{OAuthToken: oauthTok, APIKey: apiKey}, nil), []string{phKey},
			want{503, policy.CodeNoLogin, "", "", "", ClientPlaceholder}},
		{"no login, no carrier", anthropicFrom(AnthropicAuth{}, nil), nil,
			want{200, "", "", "", "", ClientNone}},
		{"not configured", nil, []string{phBearer},
			want{200, "", "", "", "", ClientPlaceholder}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, got := startIntercept(t, modelUpstream, func(c *Config) { c.Anthropic = tt.auth })
			for _, host := range policy.AnthropicHosts() {
				before := len(got.all())
				status, code, rec := e.call(t, host, req(host, "/v1/messages", tt.headers...))
				if status != tt.want.status || code != tt.want.code {
					t.Fatalf("%s: got %d %s, want %d %s", host, status, code, tt.want.status, tt.want.code)
				}
				if rec.Injected != tt.want.injected || rec.Client != tt.want.cli || rec.Refusal != tt.want.code {
					t.Errorf("%s: record %+v", host, rec)
				}
				if tt.want.code != "" {
					if len(got.all()) != before {
						t.Errorf("%s: a refused request reached the upstream", host)
					}
					continue
				}
				r := got.all()[before]
				if a, k := r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"); a != tt.want.auth || k != tt.want.key {
					t.Errorf("%s: upstream got Authorization %q, X-Api-Key %q", host, a, k)
				}
			}
		})
	}
}

// TestChatGPTRoutes: the account id on every chatgpt.com request, the access token only with
// Authorization, carriers stripped, the infrastructure cookies kept, a 401 passed on, and
// no-login without a token.
func TestChatGPTRoutes(t *testing.T) {
	login := chatgptFrom(ChatGPTLogin{AccessToken: accessTok, AccountID: accountID}, nil)
	tests := []struct {
		name                  string
		login                 func(context.Context) (ChatGPTLogin, error)
		path                  string
		headers               []string
		status                int
		code                  policy.Code
		auth, account, cookie string
		injected, client      string
	}{
		{name: "placeholder bearer", login: login, path: "/backend-api/codex/responses",
			headers: []string{phBearer, "ChatGPT-Account-Id: vm-account\r\n", "chatgpt-account-id: vm-account-2\r\n", "Cookie: __oailb=lb; _cfuvid=u; session=header-secret\r\n"},
			status:  200, auth: "Bearer " + accessTok, account: accountID, cookie: "__oailb=lb; _cfuvid=u",
			injected: InjectedChatGPT, client: ClientPlaceholder},
		{name: "a credential in the VM", login: login, path: "/backend-api/x", headers: []string{realAuth, phKey},
			status: 200, auth: "Bearer " + accessTok, account: accountID, injected: InjectedChatGPT, client: ClientOther},
		{name: "no Authorization", login: login, path: "/backend-api/x", headers: []string{phKey, "Cookie: session=header-secret\r\n"},
			status: 200, account: accountID, client: ClientPlaceholder},
		{name: "an upstream 401 is passed on", login: login, path: "/401", headers: []string{phBearer},
			status: 401, auth: "Bearer " + accessTok, account: accountID, injected: InjectedChatGPT, client: ClientPlaceholder},
		{name: "a stale token with an error", login: chatgptFrom(ChatGPTLogin{AccessToken: accessTok}, errors.New("store: stale")),
			path: "/backend-api/x", headers: []string{phBearer}, status: 200, auth: "Bearer " + accessTok,
			injected: InjectedChatGPT, client: ClientPlaceholder},
		{name: "no login", login: chatgptFrom(ChatGPTLogin{AccountID: accountID}, errors.New("store: none")), path: "/backend-api/x",
			headers: []string{phBearer}, status: 503, code: policy.CodeNoLogin, client: ClientPlaceholder},
		{name: "no login, no Authorization", login: chatgptFrom(ChatGPTLogin{}, nil), path: "/backend-api/x",
			status: 503, code: policy.CodeNoLogin, client: ClientNone},
		{name: "not configured", path: "/backend-api/x", headers: []string{phBearer, "Cookie: _cfuvid=u\r\n"},
			status: 200, cookie: "_cfuvid=u", client: ClientPlaceholder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, got := startIntercept(t, modelUpstream, func(c *Config) { c.ChatGPT = tt.login })
			status, code, rec := e.call(t, policy.ChatGPTHost, req(policy.ChatGPTHost, tt.path, tt.headers...))
			if status != tt.status || code != tt.code {
				t.Fatalf("got %d %s, want %d %s", status, code, tt.status, tt.code)
			}
			if rec.Status != tt.status || rec.Injected != tt.injected || rec.Client != tt.client || rec.Refusal != tt.code {
				t.Errorf("record %+v", rec)
			}
			if tt.code != "" {
				if n := len(got.all()); n != 0 {
					t.Errorf("a refused request reached the upstream %d times", n)
				}
				return
			}
			r := got.all()[0]
			if a := r.Header.Get("Authorization"); a != tt.auth {
				t.Errorf("upstream got Authorization %q, want %q", a, tt.auth)
			}
			if a := r.Header.Values(ChatGPTAccountHeader); strings.Join(a, ",") != tt.account {
				t.Errorf("upstream got %s %q, want %q", ChatGPTAccountHeader, a, tt.account)
			}
			if k := r.Header.Get("X-Api-Key"); k != "" {
				t.Errorf("upstream got X-Api-Key %q", k)
			}
			if c := strings.Join(r.Header.Values("Cookie"), "; "); c != tt.cookie {
				t.Errorf("upstream got Cookie %q, want %q", c, tt.cookie)
			}
		})
	}
}

// TestChatGPTWebSocket: codex's WebSocket upgrade on chatgpt.com carries the bearer and the
// account id, and its bytes pass both ways.
func TestChatGPTWebSocket(t *testing.T) {
	e, got := startIntercept(t, func(w http.ResponseWriter, r *http.Request) {
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(c, brw)
	}, func(c *Config) {
		c.ChatGPT = chatgptFrom(ChatGPTLogin{AccessToken: accessTok, AccountID: accountID}, nil)
	})
	tc, err := e.dialTLS(t, policy.ChatGPTHost, policy.ChatGPTHost)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(tc)
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(tc, req(policy.ChatGPTHost, "/backend-api/codex/responses", "Connection: Upgrade\r\n",
		"Upgrade: websocket\r\n", phBearer, "Cookie: __cf_bm=b; session=header-secret\r\n"))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(tc, "frame")
	b := make([]byte, 5)
	if _, err := io.ReadFull(br, b); err != nil || string(b) != "frame" {
		t.Fatalf("echo %q, %v", b, err)
	}
	r := got.all()[0]
	if a, id, c := r.Header.Get("Authorization"), r.Header.Get(ChatGPTAccountHeader), r.Header.Get("Cookie"); a != "Bearer "+accessTok || id != accountID || c != "__cf_bm=b" {
		t.Errorf("upstream got Authorization %q, %s %q, Cookie %q", a, ChatGPTAccountHeader, id, c)
	}
	_ = tc.Close()
	e.close(t)
	if rec := last(t, e.sink.all()); rec.Status != http.StatusSwitchingProtocols || rec.Injected != InjectedChatGPT || rec.Client != ClientPlaceholder {
		t.Errorf("record %+v", rec)
	}
}

// TestAuthOpenAIRefused: the VM's own ChatGPT refresh never leaves the gateway, over CONNECT or
// plain HTTP, even with a login configured.
func TestAuthOpenAIRefused(t *testing.T) {
	e := start(t, func(c *Config) {
		c.ChatGPT = chatgptFrom(ChatGPTLogin{AccessToken: accessTok, AccountID: accountID}, nil)
	})
	for _, raw := range []string{
		"CONNECT auth.openai.com:443 HTTP/1.1\r\nHost: auth.openai.com:443\r\n\r\n",
		"POST http://auth.openai.com/oauth/token HTTP/1.1\r\nHost: auth.openai.com\r\nContent-Length: 0\r\n\r\n",
	} {
		rp := e.send(t, raw)
		if rp.status != http.StatusForbidden || rp.code != policy.CodeAuthHost {
			t.Errorf("%q: got %d %s", raw, rp.status, rp.code)
		}
	}
	if n := e.res.count(policy.AuthOpenAIHost); n != 0 {
		t.Errorf("auth.openai.com was resolved %d times", n)
	}
}

func TestClassifyClient(t *testing.T) {
	ph := harness.Placeholder
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name    string
		headers map[string][]string
		user    *url.Userinfo
		want    string
	}{
		{"none", nil, nil, ClientNone},
		{"cookies don't count", map[string][]string{"Cookie": {"session=x"}}, nil, ClientNone},
		{"empty values", map[string][]string{"Authorization": {" "}, "X-Api-Key": {""}}, nil, ClientNone},
		{"bearer", map[string][]string{"Authorization": {"Bearer " + ph}}, nil, ClientPlaceholder},
		{"token", map[string][]string{"Authorization": {"token " + ph}}, nil, ClientPlaceholder},
		{"bare", map[string][]string{"Authorization": {ph}}, nil, ClientPlaceholder},
		{"basic x-access-token", map[string][]string{"Authorization": {"Basic " + b64("x-access-token:"+ph)}}, nil, ClientPlaceholder},
		{"basic placeholder user", map[string][]string{"Proxy-Authorization": {"basic " + b64(ph+":"+ph)}}, nil, ClientPlaceholder},
		{"basic token user", map[string][]string{"Authorization": {"Basic " + b64("ghp_x:"+ph)}}, nil, ClientOther},
		{"basic bad base64", map[string][]string{"Authorization": {"Basic !!"}}, nil, ClientOther},
		{"x-api-key", map[string][]string{"X-Api-Key": {ph}}, nil, ClientPlaceholder},
		{"x-api-key other", map[string][]string{"X-Api-Key": {"sk-ant-x"}}, nil, ClientOther},
		{"one other wins", map[string][]string{"Authorization": {"Bearer " + ph, "Bearer sk"}}, nil, ClientOther},
		{"userinfo placeholder", nil, url.UserPassword("x", ph), ClientPlaceholder},
		{"userinfo other", nil, url.UserPassword("x", "sk"), ClientOther},
		{"userinfo user only", nil, url.User("x"), ClientOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}, URL: &url.URL{User: tt.user}}
			for k, vs := range tt.headers {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if got := classifyClient(r); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestModelLoginsRedact: no format verb and no slog handler prints a model credential.
func TestModelLoginsRedact(t *testing.T) {
	var logs strings.Builder
	l := slog.New(slog.NewJSONHandler(&logs, nil))
	for _, v := range []any{AnthropicAuth{OAuthToken: oauthTok, APIKey: apiKey}, ChatGPTLogin{AccessToken: accessTok, AccountID: accountID}} {
		s := fmt.Sprintf("%v %+v %#v %s %q", v, v, v, v, v)
		l.Info("login", "v", v)
		for _, out := range []string{s, logs.String()} {
			if strings.Contains(out, secrets[0]) || strings.Contains(out, accountID) {
				t.Errorf("printed a credential: %s", out)
			}
		}
	}
}
