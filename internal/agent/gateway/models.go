package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/agent/harness"
)

// The model credential classes a record's Injected names.
const (
	InjectedAnthropicOAuth = "anthropic-oauth" // AnthropicAuth.OAuthToken as Bearer
	InjectedAnthropicKey   = "anthropic-key"   // AnthropicAuth.APIKey as X-Api-Key
	InjectedChatGPT        = "chatgpt"         // ChatGPTLogin.AccessToken as Bearer on chatgpt.com
)

// ChatGPTAccountHeader is the header chatgpt.com gets the login's account id in.
const ChatGPTAccountHeader = "ChatGPT-Account-Id"

// The client classes: what the VM sent in its credential carriers (Record.Client).
const (
	ClientNone        = "none"        // no carrier
	ClientPlaceholder = "placeholder" // every carrier holds harness.Placeholder
	ClientOther       = "other"       // a carrier held something else: a credential reached the VM
)

// AnthropicAuth is the Claude credential the Anthropic hosts get: exactly one of the OAuth
// token (a subscription login) and the API key. String, Format, GoString, LogValue,
// MarshalJSON and MarshalText redact, so fmt, slog (inside a slice or map too) and encoding/json
// never print it. fmt's reflection paths call no method: %p on the value, and the value in an
// unexported field of an enclosing struct, print the token. Pass it by pointer or hold it only in
// exported fields.
type AnthropicAuth struct{ OAuthToken, APIKey string }

func (AnthropicAuth) String() string { return "gateway.AnthropicAuth{redacted}" }

// Format redacts every verb, %#v included.
func (a AnthropicAuth) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, a.String()) }

// GoString redacts.
func (a AnthropicAuth) GoString() string { return a.String() }

// LogValue redacts.
func (a AnthropicAuth) LogValue() slog.Value { return slog.StringValue(a.String()) }

// MarshalJSON is the redacted form as a JSON string.
func (a AnthropicAuth) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// MarshalText is the redacted form (slog's text handler, encoding/xml, map keys).
func (a AnthropicAuth) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// ChatGPTLogin is the codex login chatgpt.com gets: the access token and the account id. It
// redacts as AnthropicAuth does, with the same limits: pass it by pointer or hold it only in
// exported fields.
type ChatGPTLogin struct{ AccessToken, AccountID string }

func (ChatGPTLogin) String() string { return "gateway.ChatGPTLogin{redacted}" }

// Format redacts every verb, %#v included.
func (l ChatGPTLogin) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, l.String()) }

// GoString redacts.
func (l ChatGPTLogin) GoString() string { return l.String() }

// LogValue redacts.
func (l ChatGPTLogin) LogValue() slog.Value { return slog.StringValue(l.String()) }

// MarshalJSON is the redacted form as a JSON string.
func (l ChatGPTLogin) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// MarshalText is the redacted form (slog's text handler, encoding/xml, map keys).
func (l ChatGPTLogin) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// routeCredentials is the class's rule and the headers to set after the carriers are stripped:
// the model hosts' (anthropicRoute, chatgptRoute), else routeIntercepted's Authorization. It answers a refusal
// itself and returns ok false then.
func (g *Gateway) routeCredentials(w http.ResponseWriter, r *http.Request, d policy.Decision, rec *Record) (http.Header, bool) {
	if d.Class == policy.Anthropic || d.Class == policy.ChatGPT {
		// Before a credential is read: TRACE reflects the request, the injected header included,
		// and a path off the host's list could read the owner's data with it.
		carrier := hasHeader(r, "Authorization") || hasHeader(r, "X-Api-Key")
		if c := policy.DecideModel(d.Target.Host, r.Method, r.URL, carrier); c != "" {
			rec.Status, rec.Refusal = writeRefusal(w, c, "")
			return nil, false
		}
	}
	switch d.Class {
	case policy.Anthropic:
		return g.anthropicRoute(w, r, rec)
	case policy.ChatGPT:
		return g.chatgptRoute(w, r, rec)
	}
	auth, ok := g.routeIntercepted(w, r, d, rec)
	if !ok || auth == "" {
		return nil, ok
	}
	return http.Header{"Authorization": {auth}}, true
}

// anthropicRoute is the Anthropic presence rule (gateway.md, the host table; D4): a request
// that carried Authorization or X-Api-Key, whatever its value, gets the gateway's credential in
// the gateway's form (Bearer for the OAuth token, X-Api-Key for the key), not the carrier the VM
// used; one without either goes on without one, as claude's event logging and MCP registry calls
// do. Without Config.Anthropic nothing is added; when it has no credential, a request with a
// carrier is no-login.
func (g *Gateway) anthropicRoute(w http.ResponseWriter, r *http.Request, rec *Record) (http.Header, bool) {
	if g.cfg.Anthropic == nil || !hasHeader(r, "Authorization") && !hasHeader(r, "X-Api-Key") {
		return nil, true
	}
	a, err := g.cfg.Anthropic(r.Context())
	if err != nil {
		g.log.Warn("gateway: the Anthropic login", "err", err)
	}
	set := http.Header{}
	switch {
	case a.OAuthToken != "" && a.APIKey == "":
		set.Set("Authorization", "Bearer "+a.OAuthToken)
		rec.Injected = InjectedAnthropicOAuth
	case a.APIKey != "" && a.OAuthToken == "":
		set.Set("X-Api-Key", a.APIKey)
		rec.Injected = InjectedAnthropicKey
	default: // none, or both: the supervisor's mistake, never a guess
		rec.Status, rec.Refusal = writeRefusal(w, policy.CodeNoLogin, "")
		return nil, false
	}
	return set, true
}

// chatgptRoute is chatgpt.com's rule (gateway.md, the host table; D2): the login's account id
// on every request, and its access token as Bearer when the request carried Authorization.
// Without Config.ChatGPT nothing is added; when it has no access token, every request is
// no-login. A 401 is the client's to see: the PoC has no refresh, the owner re-imports the login.
func (g *Gateway) chatgptRoute(w http.ResponseWriter, r *http.Request, rec *Record) (http.Header, bool) {
	if g.cfg.ChatGPT == nil {
		return nil, true
	}
	l, err := g.cfg.ChatGPT(r.Context())
	if err != nil { // a stale token still goes: its 401 shows in the record
		g.log.Warn("gateway: the ChatGPT login", "err", err)
	}
	if l.AccessToken == "" {
		rec.Status, rec.Refusal = writeRefusal(w, policy.CodeNoLogin, "")
		return nil, false
	}
	set := http.Header{}
	if l.AccountID != "" {
		set.Set(ChatGPTAccountHeader, l.AccountID)
	}
	if hasHeader(r, "Authorization") {
		set.Set("Authorization", "Bearer "+l.AccessToken)
		rec.Injected = InjectedChatGPT
	}
	return set, true
}

// hasHeader reports whether r carries the header at all, whatever its value.
func hasHeader(r *http.Request, name string) bool { return len(r.Header.Values(name)) > 0 }

// classifyClient is what r carried in its credential carriers (Authorization,
// Proxy-Authorization, X-Api-Key, URL user info), in the forms the fakegateway's classify
// accepts as a placeholder: Bearer, Token or a bare value of harness.Placeholder; Basic with it
// as the password and an empty, x, x-access-token or placeholder user; an X-Api-Key of it. An
// empty value carries nothing; Cookie doesn't count. Only the class is kept, never a value.
func classifyClient(r *http.Request) string {
	out := ClientNone
	note := func(placeholder bool) {
		switch {
		case !placeholder:
			out = ClientOther
		case out == ClientNone:
			out = ClientPlaceholder
		}
	}
	for _, k := range []string{"Authorization", "Proxy-Authorization"} {
		for _, v := range r.Header.Values(k) {
			if v = strings.TrimSpace(v); v != "" {
				note(authPlaceholder(v))
			}
		}
	}
	for _, v := range r.Header.Values("X-Api-Key") {
		if v = strings.TrimSpace(v); v != "" {
			note(v == harness.Placeholder)
		}
	}
	if u := r.URL.User; u != nil {
		pass, _ := u.Password()
		note(basicPlaceholder(u.Username(), pass))
	}
	return out
}

// authPlaceholder reports whether an Authorization value carries only the placeholder.
func authPlaceholder(v string) bool {
	scheme, cred, ok := strings.Cut(v, " ")
	if !ok {
		return v == harness.Placeholder
	}
	cred = strings.TrimSpace(cred)
	if strings.EqualFold(scheme, "basic") {
		dec, err := base64.StdEncoding.DecodeString(cred)
		user, pass, ok := strings.Cut(string(dec), ":")
		return err == nil && ok && basicPlaceholder(user, pass)
	}
	return cred == harness.Placeholder
}

// basicPlaceholder: the password is the placeholder and the user a known non-secret name or the
// placeholder too (GitHub takes a token as the user name, so any other user is a credential).
func basicPlaceholder(user, pass string) bool {
	if pass != harness.Placeholder {
		return false
	}
	switch user {
	case "", "x", "x-access-token", harness.Placeholder:
		return true
	}
	return false
}
