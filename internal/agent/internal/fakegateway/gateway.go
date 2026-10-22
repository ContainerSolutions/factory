// Package fakegateway is a test-only stand-in for phase 4's gateway (P1 §3): an explicit
// forward proxy that injects every credential, so the workspace under test never holds one.
//
//   - Absolute-form HTTP to the dispatcher (runbundle.DefaultDispatcherURL's host): the client's
//     Authorization is replaced by Bearer <run token> and the request forwarded to the test
//     dispatcher.
//   - CONNECT to github.com or api.github.com: TLS is terminated with a leaf from a per-instance
//     CA. git smart HTTP gets Basic x-access-token:<token> for the repo in the path, and a push
//     (git-receive-pack) to a repo that isn't rw is refused with a 403. api.github.com gets the
//     API token on GET and HEAD; every other method is refused with a 403.
//   - Any other CONNECT is tunnelled as it is, or refused, by Config.AllowTunnel.
//
// The client's Authorization is always replaced, never matched. Every request is recorded with
// the credential it got and the inbound Authorization's classification, never a value.
package fakegateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
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
	PushRefused     = "fakegateway: push refused: the repository is not writable for this task"
	APIWriteRefused = "fakegateway: refused: api.github.com is read-only for workspaces"
	TunnelRefused   = "fakegateway: refused: tunnels are not allowed"
	PlainRefused    = "fakegateway: refused: plain HTTP to an intercepted host"
	BadPath         = "fakegateway: refused: the path is not canonical"
	NotProxyRequest = "fakegateway: not a proxy request"
)

// Record.Injected values; a git request records "git:<owner>/<repo>".
const (
	InjectedNone     = ""
	InjectedRunToken = "run-token"
	InjectedAPI      = "api-token"
)

// ClientAuthNone is Record.ClientAuth for a request without an Authorization header. Otherwise
// it is "<scheme> placeholder" or "<scheme> other", with scheme one of basic, bearer or token
// (anything else is "unknown").
const ClientAuthNone = "none"

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
	// GitUpstream serves GitHost's git smart HTTP (a git http-backend server).
	GitHost     string
	GitUpstream string
	// Repos maps owner/name to its credential. A repo that isn't listed is fetched anonymously
	// and can't be pushed to.
	Repos map[string]Repo
	// APIUpstream serves APIHost's GETs, with APIToken injected.
	APIHost     string
	APIUpstream string
	APIToken    string
	// Placeholders are the Authorization values that aren't credentials; default
	// {DefaultPlaceholder}.
	Placeholders []string
	// AllowTunnel tunnels CONNECTs (and forwards plain HTTP) to any other host; else they get 403.
	AllowTunnel bool
	// CA signs the intercepted hosts' leaves; default a new one.
	CA *CA
}

// Record is one request the gateway saw. It holds no credential values.
type Record struct {
	Method string
	// Host is the request's host name, without the port.
	Host string
	// Path is the URL path, without the query; empty for a CONNECT.
	Path string
	// Injected names the credential the gateway added (Injected* or git:<repo>).
	Injected string
	// Status is the status the client got.
	Status int
	// ClientAuth classifies the inbound Authorization (ClientAuthNone, "<scheme> placeholder",
	// "<scheme> other").
	ClientAuth string
}

// Gateway is a running fake gateway.
type Gateway struct {
	cfg          Config
	ca           *CA
	dispatcher   *url.URL
	git, api     *url.URL
	repos        map[string]Repo
	placeholders map[string]bool
	transport    *http.Transport
	client       *http.Client

	srv   *httptest.Server
	inner *http.Server
	ln    *connListener

	mu      sync.Mutex
	records []Record
	tunnels map[net.Conn]struct{}
	closed  bool
	wg      sync.WaitGroup
	once    sync.Once
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
	cfg.DispatcherHost, cfg.GitHost, cfg.APIHost = strings.ToLower(cfg.DispatcherHost), strings.ToLower(cfg.GitHost), strings.ToLower(cfg.APIHost)
	if cfg.Placeholders == nil {
		cfg.Placeholders = []string{DefaultPlaceholder}
	}
	g := &Gateway{cfg: cfg, ca: cfg.CA, repos: map[string]Repo{}, placeholders: map[string]bool{}, tunnels: map[net.Conn]struct{}{},
		transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 16}}
	for _, p := range cfg.Placeholders {
		g.placeholders[p] = true
	}
	for name, r := range cfg.Repos {
		g.repos[strings.ToLower(name)] = r
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
	go func() { _ = g.inner.Serve(g.ln) }()
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	g.srv.Config.ErrorLog = discardLog()
	g.srv.Start()
	u, _ := url.Parse(g.srv.URL) // httptest's own URL always parses
	g.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u),
		TLSClientConfig: &tls.Config{RootCAs: g.ca.Pool(), MinVersion: tls.VersionTLS12}}}
	return g, nil
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
		return nil, fmt.Errorf("fakegateway: upstream %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("fakegateway: upstream %q is not an absolute http(s) URL", raw)
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

// Close stops the gateway and closes every tunnel and intercepted connection. It is idempotent.
func (g *Gateway) Close() {
	g.once.Do(func() {
		g.srv.Close()
		_ = g.inner.Close()
		g.ln.close()
		g.mu.Lock()
		g.closed = true
		for c := range g.tunnels {
			_ = c.Close()
		}
		g.mu.Unlock()
		g.wg.Wait()
		g.transport.CloseIdleConnections()
		g.client.CloseIdleConnections()
	})
}

// Records is a copy of what the gateway has seen so far, in order.
func (g *Gateway) Records() []Record {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Record(nil), g.records...)
}

// AssertNoClientCredential fails t when any request arrived with an Authorization that was
// neither absent nor a placeholder: the workspace held a credential.
func (g *Gateway) AssertNoClientCredential(t testing.TB) {
	t.Helper()
	for _, r := range g.Records() {
		if r.ClientAuth != ClientAuthNone && !strings.HasSuffix(r.ClientAuth, " placeholder") {
			t.Errorf("fakegateway: %s %s%s arrived with a client credential (%s)", r.Method, r.Host, r.Path, r.ClientAuth)
		}
	}
}

func (g *Gateway) record(r Record) {
	g.mu.Lock()
	g.records = append(g.records, r)
	g.mu.Unlock()
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
	name := "unknown"
	switch s := strings.ToLower(scheme); s {
	case "basic":
		name = s
		if dec, err := base64.StdEncoding.DecodeString(cred); err == nil {
			if _, pass, ok := strings.Cut(string(dec), ":"); ok {
				cred = pass
			}
		}
	case "bearer", "token":
		name = s
	}
	if g.placeholders[cred] {
		return name + " placeholder"
	}
	return name + " other"
}

// serve is the proxy's own handler: CONNECT and absolute-form HTTP.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.connect(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	rec := Record{Method: r.Method, Host: r.URL.Hostname(), Path: r.URL.Path, ClientAuth: g.classify(r.Header.Get("Authorization"))}
	defer func() { rec.Status = sw.status(); g.record(rec) }()
	switch host := strings.ToLower(r.URL.Hostname()); {
	case !r.URL.IsAbs():
		rec.Host = r.Host
		http.Error(sw, NotProxyRequest, http.StatusBadRequest)
	case host == g.cfg.DispatcherHost:
		auth := ""
		if g.cfg.RunToken != "" {
			auth, rec.Injected = "Bearer "+g.cfg.RunToken, InjectedRunToken
		}
		g.forward(sw, r, g.dispatcher, replaceAuth(auth))
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
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	host = strings.ToLower(host)
	rec := Record{Method: r.Method, Host: host, ClientAuth: g.classify(r.Header.Get("Authorization"))}
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
	g.tunnels[client], g.tunnels[up] = struct{}{}, struct{}{}
	g.wg.Add(2)
	g.mu.Unlock()
	done := func() {
		_ = client.Close()
		_ = up.Close()
		g.mu.Lock()
		delete(g.tunnels, client)
		delete(g.tunnels, up)
		g.mu.Unlock()
		g.wg.Done()
	}
	go func() { defer done(); _, _ = io.Copy(up, client) }()
	go func() { defer done(); _, _ = io.Copy(client, up) }()
}

// intercepted serves the requests inside an intercepted CONNECT.
func (g *Gateway) intercepted(w http.ResponseWriter, r *http.Request) {
	host, _ := r.Context().Value(hostKey{}).(string)
	sw := &statusWriter{ResponseWriter: w}
	rec := Record{Method: r.Method, Host: host, Path: r.URL.Path, ClientAuth: g.classify(r.Header.Get("Authorization"))}
	defer func() { rec.Status = sw.status(); g.record(rec) }()
	if r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path {
		http.Error(sw, BadPath, http.StatusBadRequest)
		return
	}
	switch host {
	case g.cfg.GitHost:
		repo, rest := splitRepo(r.URL.Path)
		cred, known := g.repos[repo]
		push := rest == "git-receive-pack" || (rest == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack")
		if push && (!known || !cred.RW) {
			http.Error(sw, PushRefused, http.StatusForbidden)
			return
		}
		auth := ""
		if known && cred.Token != "" {
			auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+cred.Token))
			rec.Injected = "git:" + repo
		}
		g.forward(sw, r, g.git, replaceAuth(auth))
	case g.cfg.APIHost:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(sw, APIWriteRefused, http.StatusForbidden)
			return
		}
		auth := ""
		if g.cfg.APIToken != "" {
			auth, rec.Injected = "Bearer "+g.cfg.APIToken, InjectedAPI
		}
		g.forward(sw, r, g.api, replaceAuth(auth))
	default:
		http.Error(sw, NotProxyRequest, http.StatusBadRequest)
	}
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
	return strings.ToLower(parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")), rest
}

// replaceAuth drops the client's Authorization and sets auth, if any.
func replaceAuth(auth string) func(http.Header) {
	return func(h http.Header) {
		h.Del("Authorization")
		if auth != "" {
			h.Set("Authorization", auth)
		}
	}
}

// forward sends r to upstream (its scheme and host, the path joined), after setAuth.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, upstream *url.URL, setAuth func(http.Header)) {
	if upstream == nil {
		http.Error(w, "fakegateway: no upstream for this host", http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport: g.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			if setAuth != nil {
				setAuth(pr.Out.Header)
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
