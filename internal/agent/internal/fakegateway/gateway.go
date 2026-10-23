// Package fakegateway is a test-only stand-in for phase 4's gateway (P1 §3): an explicit
// forward proxy that injects every credential, so the workspace under test never holds one.
//
//   - Absolute-form HTTP to the dispatcher (runbundle.DefaultDispatcherURL's host): only the
//     Config.DispatcherRoutes (default POST /api/v1/tasks/{id}/effects) are forwarded, with the
//     client's Authorization replaced by Bearer <run token>; any other method or path gets a 403,
//     and a path that isn't canonical a 400.
//   - CONNECT to github.com or api.github.com: TLS is terminated with a leaf from a per-instance
//     CA. git smart HTTP gets Basic x-access-token:<token> for the repo in the path. A repo that
//     isn't rw fails closed: only a smart-HTTP fetch (GET info/refs?service=git-upload-pack and
//     POST git-upload-pack) is forwarded, and every other request gets a 403. api.github.com gets
//     the API token on GET and HEAD; every other method is refused with a 403.
//   - Any other CONNECT is tunnelled as it is, or refused, by Config.AllowTunnel. The tunnel dials
//     any address: P1 §3's egress deny list (169.254.0.0/16, RFC 1918, the agent host's own
//     addresses, other workspaces) belongs to phase 4's gateway and the network layer, and isn't
//     modelled here.
//
// Host names are matched lower-cased and without a trailing dot. The client's Authorization is
// always replaced, never matched, and its other credential carriers (Proxy-Authorization,
// X-Api-Key, Cookie, URL userinfo) are dropped on the injected routes. Every request is recorded
// with the credential it got and the classification of each credential it arrived with, never a
// value.
package fakegateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runbundle"
)

const (
	// RunToken is a run token in the API's shape (rtk_ and 43 base64url characters) for tests.
	RunToken = "rtk_fakegatewayRunTokenNeverInTheWorkspace01234"
	// DefaultPlaceholder is the placeholder credential the workspace carries
	// (harness.Placeholder in the LLD's harness section).
	DefaultPlaceholder = "factory-placeholder-not-a-credential"
	// DefaultGitHost and DefaultAPIHost are the intercepted GitHub hosts.
	DefaultGitHost = "github.com"
	DefaultAPIHost = "api.github.com"
)

// The bodies of the gateway's refusals, which tests and the workspace's logs recognise.
const (
	PushRefused       = "fakegateway: push refused: the repository is not writable for this task"
	APIWriteRefused   = "fakegateway: refused: api.github.com is read-only for workspaces"
	DispatcherRefused = "fakegateway: refused: the dispatcher route is not open to workspaces"
	TunnelRefused     = "fakegateway: refused: tunnels are not allowed"
	PlainRefused      = "fakegateway: refused: plain HTTP to an intercepted host"
	BadPath           = "fakegateway: refused: the path is not canonical"
	NotProxyRequest   = "fakegateway: not a proxy request"
)

// ErrConfig is wrapped by New's errors for a Config it rejects.
var ErrConfig = errors.New("fakegateway: invalid config")

// Record.Injected values; a git request records "git:<owner>/<repo>".
const (
	InjectedNone     = ""
	InjectedRunToken = "run-token"
	InjectedAPI      = "api-token"
)

// ClientAuthNone is Record.ClientAuth for a request without an Authorization header. Otherwise
// it is "<scheme> placeholder" or "<scheme> other", with scheme one of basic, bearer or token
// (anything else is "unknown"). Basic is a placeholder only when its password is one and its
// username is empty, x, x-access-token or a placeholder: a token as the username is "other".
const ClientAuthNone = "none"

// Route is a dispatcher route the gateway forwards: an HTTP method and a canonical path whose
// segment {id} is Config.TaskID (any one non-empty segment when TaskID is empty). The query
// isn't matched.
type Route struct {
	Method string
	Path   string
}

// DefaultDispatcherRoutes are the routes a run's VM needs (P1 §3): its effects only.
func DefaultDispatcherRoutes() []Route {
	return []Route{{Method: http.MethodPost, Path: "/api/v1/tasks/{id}/effects"}}
}

// Repo is what the gateway may inject for one repository.
type Repo struct {
	Token string
	RW    bool
}

// Config configures a Gateway. Hosts default to the real names; an upstream that's empty makes
// that route answer 502.
type Config struct {
	// RunToken replaces Authorization on the dispatcher route.
	RunToken string
	// Dispatcher is the upstream URL of DispatcherHost (the test dispatcher).
	Dispatcher     string
	DispatcherHost string
	// TaskID binds {id} in DispatcherRoutes; empty matches any task.
	TaskID string
	// DispatcherRoutes are the only dispatcher requests forwarded; default
	// DefaultDispatcherRoutes(). Everything else gets a 403 with DispatcherRefused.
	DispatcherRoutes []Route
	// GitUpstream serves GitHost's git smart HTTP (a git http-backend server).
	GitHost     string
	GitUpstream string
	// Repos maps owner/name to its credential. Keys are case-insensitive and may end in .git; a
	// key that isn't owner/name, or two keys that are the same repo, are an error. A repo that
	// isn't listed is fetched anonymously and can't be pushed to.
	Repos map[string]Repo
	// APIUpstream serves APIHost's GETs, with APIToken injected.
	APIHost     string
	APIUpstream string
	APIToken    string
	// Placeholders are the Authorization values that aren't credentials; default
	// {DefaultPlaceholder}.
	Placeholders []string
	// AllowTunnel tunnels CONNECTs (and forwards plain HTTP) to any other host, at any address
	// (no egress deny list); else they get 403.
	AllowTunnel bool
	// CA signs the intercepted hosts' leaves; default a new one.
	CA *CA
}

// Record is one request the gateway saw. It holds no credential values.
type Record struct {
	Method string
	// Host is the request's host name, lower-cased and without the port.
	Host string
	// Path is the URL path, without the query; empty for a CONNECT.
	Path string
	// Injected names the credential the gateway added (Injected* or git:<repo>).
	Injected string
	// Status is the status the client got.
	Status int
	// ClientAuth classifies the inbound Authorization (ClientAuthNone, "<scheme> placeholder",
	// "<scheme> other"). With several values, the first "other" wins.
	ClientAuth string
	// Credentials classifies the request's other credential carriers, comma-separated in this
	// order and only those present: "proxy-authorization <class>" (classified as ClientAuth),
	// "x-api-key placeholder|other", "cookie other" and "userinfo basic placeholder|other" (an
	// absolute-form URL's). Empty when there were none.
	Credentials string
}

// Gateway is a running fake gateway.
type Gateway struct {
	cfg          Config
	ca           *CA
	dispatcher   *url.URL
	git, api     *url.URL
	repos        map[string]Repo
	routes       []route
	placeholders map[string]bool
	transport    *http.Transport
	client       *http.Client

	srv   *httptest.Server
	inner *http.Server
	ln    *connListener

	mu      sync.Mutex
	records []Record
	// conns are the hijacked connections, tunnelled or still in their CONNECT handler: Close
	// closes them. wg counts the goroutines that may record or hold one: the inner server, the
	// CONNECT handlers after their hijack, the intercepted handlers and the tunnels' copies.
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
	once   sync.Once
}

// route is a Route with its path split on "/".
type route struct {
	method string
	segs   []string
}

// New starts a gateway listening on 127.0.0.1.
func New(cfg Config) (*Gateway, error) {
	if cfg.DispatcherHost == "" {
		u, err := url.Parse(runbundle.DefaultDispatcherURL)
		if err != nil {
			return nil, fmt.Errorf("fakegateway: default dispatcher URL: %w", err)
		}
		cfg.DispatcherHost = u.Hostname()
	}
	if cfg.GitHost == "" {
		cfg.GitHost = DefaultGitHost
	}
	if cfg.APIHost == "" {
		cfg.APIHost = DefaultAPIHost
	}
	cfg.DispatcherHost, cfg.GitHost, cfg.APIHost = normHost(cfg.DispatcherHost), normHost(cfg.GitHost), normHost(cfg.APIHost)
	if cfg.Placeholders == nil {
		cfg.Placeholders = []string{DefaultPlaceholder}
	}
	if cfg.DispatcherRoutes == nil {
		cfg.DispatcherRoutes = DefaultDispatcherRoutes()
	}
	g := &Gateway{cfg: cfg, ca: cfg.CA, repos: map[string]Repo{}, placeholders: map[string]bool{}, conns: map[net.Conn]struct{}{},
		transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 16}}
	for _, p := range cfg.Placeholders {
		g.placeholders[p] = true
	}
	seen := map[string]string{}
	for name, r := range cfg.Repos {
		key, err := repoKey(name)
		if err != nil {
			return nil, err
		}
		if other, dup := seen[key]; dup {
			return nil, fmt.Errorf("%w: repos %q and %q are the same repo", ErrConfig, other, name)
		}
		seen[key] = name
		g.repos[key] = r
	}
	for _, rt := range cfg.DispatcherRoutes {
		r, err := parseRoute(rt)
		if err != nil {
			return nil, err
		}
		g.routes = append(g.routes, r)
	}
	var err error
	for _, u := range []struct {
		dst **url.URL
		raw string
	}{{&g.dispatcher, cfg.Dispatcher}, {&g.git, cfg.GitUpstream}, {&g.api, cfg.APIUpstream}} {
		if *u.dst, err = parseUpstream(u.raw); err != nil {
			return nil, err
		}
	}
	if g.ca == nil {
		if g.ca, err = NewCA(); err != nil {
			return nil, err
		}
	}
	g.ln = &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
	g.inner = &http.Server{Handler: http.HandlerFunc(g.intercepted), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: discardLog(),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if hc, ok := c.(*hostConn); ok {
				return context.WithValue(ctx, hostKey{}, hc.host)
			}
			return ctx
		}}
	g.wg.Add(1)
	go func() { defer g.wg.Done(); _ = g.inner.Serve(g.ln) }()
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	g.srv.Config.ErrorLog = discardLog()
	g.srv.Start()
	u, _ := url.Parse(g.srv.URL) // httptest's own URL always parses
	g.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u),
		TLSClientConfig: &tls.Config{RootCAs: g.ca.Pool(), MinVersion: tls.VersionTLS12}}}
	return g, nil
}

// normHost is a host name as the gateway matches and records it: lower-cased, without a
// trailing dot.
func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// hostOnly is normHost of a host[:port].
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return normHost(h)
	}
	return normHost(hostport)
}

// repoKey is a Repos key as splitRepo spells a path's repo: lower-cased owner/name, without .git.
func repoKey(name string) (string, error) {
	key := strings.TrimSuffix(strings.ToLower(name), ".git")
	owner, repo, ok := strings.Cut(key, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("%w: repo %q is not owner/name", ErrConfig, name)
	}
	return key, nil
}

// parseRoute checks a Route: a method, and a canonical absolute path whose only pattern is {id}.
func parseRoute(rt Route) (route, error) {
	if rt.Method == "" || strings.ContainsAny(rt.Method, " \t/") {
		return route{}, fmt.Errorf("%w: dispatcher route %q %q: not a method", ErrConfig, rt.Method, rt.Path)
	}
	if !strings.HasPrefix(rt.Path, "/") || path.Clean(rt.Path) != rt.Path {
		return route{}, fmt.Errorf("%w: dispatcher route %s %q: not a canonical absolute path", ErrConfig, rt.Method, rt.Path)
	}
	segs := strings.Split(rt.Path, "/")
	for _, s := range segs {
		if strings.ContainsAny(s, "{}") && s != "{id}" {
			return route{}, fmt.Errorf("%w: dispatcher route %s %q: only {id} is a pattern", ErrConfig, rt.Method, rt.Path)
		}
	}
	return route{method: rt.Method, segs: segs}, nil
}

// discardLog is the servers' ErrorLog: a test's aborted connections aren't news.
func discardLog() *log.Logger { return slog.NewLogLogger(slog.DiscardHandler, slog.LevelError) }

// Start is New for a test: it fails t on an error and closes the gateway in t.Cleanup.
func Start(t testing.TB, cfg Config) *Gateway {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("fakegateway: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}

func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: upstream %q: %w", ErrConfig, raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: upstream %q is not an absolute http(s) URL", ErrConfig, raw)
	}
	return u, nil
}

// URL is the proxy's URL, for HTTP_PROXY and HTTPS_PROXY.
func (g *Gateway) URL() string { return g.srv.URL }

// CA is the gateway's CA.
func (g *Gateway) CA() *CA { return g.ca }

// Client is the HTTP client that goes through the gateway and trusts only its CA. Close closes
// its idle connections.
func (g *Gateway) Client() *http.Client { return g.client }

// Close stops the gateway: it closes every tunnel and intercepted connection, including a
// CONNECT still in its TLS handshake, and waits for the handlers in flight, so nothing is
// recorded after it returns. It is idempotent.
func (g *Gateway) Close() {
	g.once.Do(func() {
		g.mu.Lock()
		g.closed = true
		for c := range g.conns {
			_ = c.Close()
		}
		g.mu.Unlock()
		g.srv.CloseClientConnections()
		g.srv.Close()
		g.ln.close()
		_ = g.inner.Close()
		g.wg.Wait()
		g.transport.CloseIdleConnections()
		g.client.CloseIdleConnections()
	})
}

// hold counts a goroutine in wg and, when c isn't nil, tracks c for Close; false once the
// gateway is closed. release undoes it.
func (g *Gateway) hold(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if c != nil {
		g.conns[c] = struct{}{}
	}
	g.wg.Add(1)
	return true
}

func (g *Gateway) release(c net.Conn) {
	if c != nil {
		g.mu.Lock()
		delete(g.conns, c)
		g.mu.Unlock()
	}
	g.wg.Done()
}

// Records is a copy of what the gateway has seen so far, in order.
func (g *Gateway) Records() []Record {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Record(nil), g.records...)
}

// AssertNoClientCredential fails t when any request arrived with a credential that was neither
// absent nor a placeholder, in its Authorization or any other carrier Record.Credentials
// classifies: the workspace held a credential.
func (g *Gateway) AssertNoClientCredential(t testing.TB) {
	t.Helper()
	for _, r := range g.Records() {
		var bad []string
		if r.ClientAuth != ClientAuthNone && !strings.HasSuffix(r.ClientAuth, " placeholder") {
			bad = append(bad, "authorization "+r.ClientAuth)
		}
		for _, c := range strings.Split(r.Credentials, ",") {
			if strings.HasSuffix(c, " other") {
				bad = append(bad, c)
			}
		}
		if len(bad) > 0 {
			t.Errorf("fakegateway: %s %s%s arrived with a client credential (%s)", r.Method, r.Host, r.Path, strings.Join(bad, ", "))
		}
	}
}

func (g *Gateway) record(r Record) {
	g.mu.Lock()
	g.records = append(g.records, r)
	g.mu.Unlock()
}

// newRecord is r's record, with every inbound credential carrier classified.
func (g *Gateway) newRecord(r *http.Request, host string) Record {
	return Record{Method: r.Method, Host: host, Path: r.URL.Path,
		ClientAuth: g.classifyAll(r.Header.Values("Authorization")), Credentials: g.credentials(r)}
}

// classifyAll classifies every value of an Authorization-like header: the first "other" wins,
// else the first value's class.
func (g *Gateway) classifyAll(vs []string) string {
	out := ClientAuthNone
	for _, v := range vs {
		c := g.classify(v)
		if strings.HasSuffix(c, " other") {
			return c
		}
		if out == ClientAuthNone {
			out = c
		}
	}
	return out
}

// credentials is Record.Credentials for r.
func (g *Gateway) credentials(r *http.Request) string {
	var out []string
	if vs := r.Header.Values("Proxy-Authorization"); len(vs) > 0 {
		out = append(out, "proxy-authorization "+g.classifyAll(vs))
	}
	if vs := r.Header.Values("X-Api-Key"); len(vs) > 0 {
		c := "placeholder"
		for _, v := range vs {
			if !g.placeholders[strings.TrimSpace(v)] {
				c = "other"
			}
		}
		out = append(out, "x-api-key "+c)
	}
	if len(r.Header.Values("Cookie")) > 0 {
		out = append(out, "cookie other")
	}
	if u := r.URL.User; u != nil {
		pass, _ := u.Password()
		c := "basic other"
		if g.basicPlaceholder(u.Username(), pass) {
			c = "basic placeholder"
		}
		out = append(out, "userinfo "+c)
	}
	return strings.Join(out, ",")
}

// classify is the inbound Authorization's scheme and whether its credential is a placeholder.
// The scheme is named only when it is a known one, so no part of a value is ever kept.
func (g *Gateway) classify(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ClientAuthNone
	}
	scheme, cred, ok := strings.Cut(v, " ")
	if !ok {
		scheme, cred = "", v
	}
	cred = strings.TrimSpace(cred)
	switch s := strings.ToLower(scheme); s {
	case "basic":
		dec, err := base64.StdEncoding.DecodeString(cred)
		if user, pass, ok := strings.Cut(string(dec), ":"); err == nil && ok && g.basicPlaceholder(user, pass) {
			return "basic placeholder"
		}
		return "basic other"
	case "bearer", "token":
		if g.placeholders[cred] {
			return s + " placeholder"
		}
		return s + " other"
	}
	if g.placeholders[cred] {
		return "unknown placeholder"
	}
	return "unknown other"
}

// basicPlaceholder is whether a Basic user and password carry no credential: the password is a
// placeholder and the user is a known non-secret name or a placeholder too. GitHub accepts a
// token as the username, so any other username counts as a credential.
func (g *Gateway) basicPlaceholder(user, pass string) bool {
	if !g.placeholders[pass] {
		return false
	}
	switch user {
	case "", "x", "x-access-token":
		return true
	}
	return g.placeholders[user]
}

// canonical is whether u's path is the one that is matched: not encoded differently and clean.
func canonical(u *url.URL) bool { return u.RawPath == "" && path.Clean(u.Path) == u.Path }

// dispatcherAllowed is whether method and the canonical path p match a dispatcher route.
func (g *Gateway) dispatcherAllowed(method, p string) bool {
	segs := strings.Split(p, "/")
	for _, rt := range g.routes {
		if rt.method != method || len(rt.segs) != len(segs) {
			continue
		}
		ok := true
		for i, want := range rt.segs {
			got := segs[i]
			if want == "{id}" {
				ok = got != "" && (g.cfg.TaskID == "" || got == g.cfg.TaskID)
			} else {
				ok = got == want
			}
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// serve is the proxy's own handler: CONNECT and absolute-form HTTP.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.connect(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	host := normHost(r.URL.Hostname())
	rec := g.newRecord(r, host)
	defer func() { rec.Status = sw.status(); g.record(rec) }()
	switch {
	case !r.URL.IsAbs():
		rec.Host = hostOnly(r.Host)
		http.Error(sw, NotProxyRequest, http.StatusBadRequest)
	case host == g.cfg.DispatcherHost:
		if !canonical(r.URL) {
			http.Error(sw, BadPath, http.StatusBadRequest)
			return
		}
		if !g.dispatcherAllowed(r.Method, r.URL.Path) {
			http.Error(sw, DispatcherRefused, http.StatusForbidden)
			return
		}
		auth := ""
		if g.cfg.RunToken != "" {
			auth, rec.Injected = "Bearer "+g.cfg.RunToken, InjectedRunToken
		}
		g.forward(sw, r, g.dispatcher, injectAuth(auth))
	case host == g.cfg.GitHost || host == g.cfg.APIHost:
		http.Error(sw, PlainRefused, http.StatusForbidden)
	case g.cfg.AllowTunnel:
		target := &url.URL{Scheme: r.URL.Scheme, Host: r.URL.Host}
		g.forward(sw, r, target, nil)
	default:
		http.Error(sw, TunnelRefused, http.StatusForbidden)
	}
}

// connect intercepts a CONNECT to GitHost or APIHost, or tunnels or refuses any other.
func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	rec := g.newRecord(r, host)
	rec.Path = ""
	intercept := host == g.cfg.GitHost || host == g.cfg.APIHost
	var up net.Conn
	switch {
	case intercept:
	case !g.cfg.AllowTunnel:
		rec.Status = http.StatusForbidden
		g.record(rec)
		http.Error(w, TunnelRefused, http.StatusForbidden)
		return
	default:
		d := net.Dialer{Timeout: 10 * time.Second}
		var err error
		if up, err = d.DialContext(r.Context(), "tcp", r.Host); err != nil {
			rec.Status = http.StatusBadGateway
			g.record(rec)
			http.Error(w, "fakegateway: tunnel: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		if up != nil {
			_ = up.Close()
		}
		rec.Status = http.StatusInternalServerError
		g.record(rec)
		http.Error(w, "fakegateway: hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The server forgets a hijacked connection: track it until a tunnel or the inner server
	// owns it, so Close closes it even mid-handshake.
	if !g.hold(conn) {
		_ = conn.Close()
		if up != nil {
			_ = up.Close()
		}
		return
	}
	defer g.release(conn)
	rec.Status = http.StatusOK
	g.record(rec)
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = conn.Close()
		if up != nil {
			_ = up.Close()
		}
		return
	}
	client := &bufConn{Conn: conn, r: brw.Reader}
	if !intercept {
		g.tunnel(client, up)
		return
	}
	tc := tls.Server(client, &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return g.ca.Leaf(host) }})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = tc.Close()
		return
	}
	g.ln.push(&hostConn{Conn: tc, host: host})
}

// tunnel copies both ways between the client and up until either side closes.
func (g *Gateway) tunnel(client, up net.Conn) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = client.Close()
		_ = up.Close()
		return
	}
	g.conns[client], g.conns[up] = struct{}{}, struct{}{}
	g.wg.Add(2)
	g.mu.Unlock()
	done := func() {
		_ = client.Close()
		_ = up.Close()
		g.mu.Lock()
		delete(g.conns, client)
		delete(g.conns, up)
		g.mu.Unlock()
		g.wg.Done()
	}
	go func() { defer done(); _, _ = io.Copy(up, client) }()
	go func() { defer done(); _, _ = io.Copy(client, up) }()
}

// intercepted serves the requests inside an intercepted CONNECT.
func (g *Gateway) intercepted(w http.ResponseWriter, r *http.Request) {
	if !g.hold(nil) {
		http.Error(w, "fakegateway: closed", http.StatusServiceUnavailable)
		return
	}
	defer g.release(nil)
	host, _ := r.Context().Value(hostKey{}).(string)
	sw := &statusWriter{ResponseWriter: w}
	rec := g.newRecord(r, host)
	defer func() { rec.Status = sw.status(); g.record(rec) }()
	if !canonical(r.URL) {
		http.Error(sw, BadPath, http.StatusBadRequest)
		return
	}
	switch host {
	case g.cfg.GitHost:
		repo, rest := splitRepo(r.URL.Path)
		cred, known := g.repos[repo]
		if (!known || !cred.RW) && !gitFetch(r, rest) {
			http.Error(sw, PushRefused, http.StatusForbidden)
			return
		}
		auth := ""
		if known && cred.Token != "" {
			auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+cred.Token))
			rec.Injected = "git:" + repo
		}
		g.forward(sw, r, g.git, injectAuth(auth))
	case g.cfg.APIHost:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(sw, APIWriteRefused, http.StatusForbidden)
			return
		}
		auth := ""
		if g.cfg.APIToken != "" {
			auth, rec.Injected = "Bearer "+g.cfg.APIToken, InjectedAPI
		}
		g.forward(sw, r, g.api, injectAuth(auth))
	default:
		http.Error(sw, NotProxyRequest, http.StatusBadRequest)
	}
}

// gitFetch is whether r is exactly a smart-HTTP fetch, the only request a repo that isn't rw
// may make: GET info/refs with the single query service=git-upload-pack, or POST git-upload-pack
// without a query. Anything else (a push, a duplicate or case-variant parameter, another path)
// is refused, so a request git http-backend reads differently can't reach it.
func gitFetch(r *http.Request, rest string) bool {
	switch {
	case r.Method == http.MethodGet && rest == "info/refs":
		return r.URL.RawQuery == "service=git-upload-pack"
	case r.Method == http.MethodPost && rest == "git-upload-pack":
		return r.URL.RawQuery == ""
	}
	return false
}

// splitRepo is the lower-cased owner/name of a git smart-HTTP path (without .git) and the rest.
func splitRepo(p string) (repo, rest string) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	if len(parts) == 3 {
		rest = parts[2]
	}
	return strings.ToLower(parts[0]) + "/" + strings.TrimSuffix(strings.ToLower(parts[1]), ".git"), rest
}

// injectAuth drops the client's credential carriers (Authorization, Proxy-Authorization,
// X-Api-Key, Cookie and URL userinfo) and sets auth, if any.
func injectAuth(auth string) func(*http.Request) {
	return func(out *http.Request) {
		for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie"} {
			out.Header.Del(h)
		}
		out.URL.User = nil
		if auth != "" {
			out.Header.Set("Authorization", auth)
		}
	}
}

// forward sends r to upstream (its scheme and host, the path joined), after setAuth.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, upstream *url.URL, setAuth func(*http.Request)) {
	if upstream == nil {
		http.Error(w, "fakegateway: no upstream for this host", http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport: g.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			if setAuth != nil {
				setAuth(pr.Out)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "fakegateway: upstream: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// statusWriter keeps the final status a handler sent.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 && code >= 200 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// bufConn reads what the server had already buffered from a hijacked connection first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

type hostKey struct{}

// hostConn is an intercepted connection and the host its CONNECT named.
type hostConn struct {
	net.Conn
	host string
}

// connListener hands intercepted connections to the inner server.
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *connListener) push(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) close() { l.once.Do(func() { close(l.done) }) }

func (l *connListener) Close() error { l.close(); return nil }

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
