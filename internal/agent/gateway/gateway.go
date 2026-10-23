// Package gateway is the workspace gateway (gateway.md): an explicit forward proxy that is the
// workspace's only TCP route out. It accepts connections only from its VM's exact addresses,
// tunnels CONNECTs and forwards absolute-form http requests after the egress check, dials every
// upstream through one egress dialer (resolve once, check every address, dial the literals), and
// writes one egress record per request. Every decision is the pure policy package's.
//
// A CONNECT to an intercepted host is TLS-terminated with the agent host's CA (package ca), and
// each request inside it is forwarded to that host over TLS with every credential carrier
// stripped; a WebSocket upgrade passes through. github.com gets the git credentials by repo
// (Basic), with every repo that isn't rw fail-closed to the two fetch forms, and api.github.com
// the task token on reads (GET, HEAD, and POST /graphql when the document only queries). The
// dispatcher's name serves only POST effects, over its own transport, with
// Config.DispatcherToken. The Anthropic hosts get Config.Anthropic's credential when the VM sent
// a carrier, and chatgpt.com Config.ChatGPT's account id and, with Authorization, its access
// token; the model hosts keep only the infrastructure cookies. Every record classifies the VM's
// carriers (none, placeholder, other) and names the credential class added, never a value.
package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/ca"
	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/clock"
)

// Config configures a Gateway (gateway.md, API).
type Config struct {
	// TaskID and RunID identify the workspace; both required.
	TaskID, RunID string
	// Clients are the VM's exact addresses (IPv4-mapped ones unmapped): any other peer is closed
	// at accept. Required.
	Clients []netip.Addr
	// CA signs the leaves of the intercepted hosts; required.
	CA *ca.CA
	// Git is the job's git credentials; nil: none. It is read on every request, so a
	// heartbeat's Set and SetReads apply at once. Only github.com credentials are used.
	Git *runner.Credentials
	// Dispatcher is the supervisor's dispatcher base URL (absolute http or https, without user
	// info, query or fragment); required. The VM reaches it as dispatcher.factory.internal.
	Dispatcher *url.URL
	// DispatcherToken returns the bearer token for the dispatcher route, read on every request;
	// required. For the PoC it is the worker's agent token (worker.Token); the lease's run token
	// (#101) when that lands. "" sends the request without one.
	DispatcherToken func() string
	// Anthropic returns the Claude credential (exactly one of its fields) the Anthropic hosts
	// get, read on every request that carried a carrier, so the supervisor hands it in from its
	// login store; nil: none (a codex task's gateway). A value returned with an error is still
	// used; none is no-login. The gateway logs its error, which must never hold a credential.
	Anthropic func(ctx context.Context) (AnthropicAuth, error)
	// ChatGPT returns the codex login chatgpt.com gets, read on every request; nil: none (a
	// claude task's gateway). A token returned with an error is still sent (its 401 shows in the
	// record); no token is no-login. The PoC doesn't refresh it: the owner re-imports the login.
	// The gateway logs its error, which must never hold a credential.
	ChatGPT func(ctx context.Context) (ChatGPTLogin, error)
	// UpstreamRoots verifies the intercepted hosts' upstreams, and an https dispatcher; default
	// the system roots.
	UpstreamRoots *x509.CertPool
	// Egress is the task's mode and allow list, extra denied prefixes and open addresses.
	Egress policy.Egress
	// Sink receives the egress log; required.
	Sink Sink
	// Clock times the records and the interface cache; default the real clock.
	Clock clock.Clock
	// Logger defaults to discarding.
	Logger *slog.Logger
	// Resolver resolves the destinations; default net.DefaultResolver.
	Resolver Resolver
}

// Sink receives every egress record. It is called synchronously, one call at a time, and never
// after Close returns; it must not block.
type Sink interface{ Egress(Record) }

// Resolver resolves a destination's name; net.DefaultResolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// The timeouts and bounds (gateway.md, "Limits and timeouts"). The PoC fixes them; the LLD's
// Limits knob is deferred.
const (
	readHeaderTimeout     = 10 * time.Second
	maxHeaderBytes        = 64 << 10
	idleTimeout           = 90 * time.Second
	maxConns              = 512 // client connections at once; more are closed at accept
	resolveTimeout        = 5 * time.Second
	dialTimeout           = 10 * time.Second
	responseHeaderTimeout = 5 * time.Minute
	keepAlive             = 30 * time.Second
)

// ErrConfig is wrapped by every *ConfigError.
var ErrConfig = errors.New("invalid gateway config")

// ConfigError is a Config field that New or Serve refuses. It never carries the value.
type ConfigError struct {
	Field string
	Err   error // a policy or v1alpha1 sentinel, when there is one
}

func (e *ConfigError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("gateway: config: %s: %v", e.Field, e.Err)
	}
	return "gateway: config: " + e.Field + " is invalid"
}

// Is matches ErrConfig.
func (e *ConfigError) Is(target error) bool { return target == ErrConfig }

func (e *ConfigError) Unwrap() error { return e.Err }

// Gateway is one workspace's proxy. New validates it, Serve runs it and Close ends it.
type Gateway struct {
	cfg     Config
	clients map[netip.Addr]bool
	log     *slog.Logger
	clk     clock.Clock
	ifaces  *hostAddrs
	// peekTimeout bounds a tunnel's ClientHello peek, and maxConns the client connections at
	// once; New sets the constants, tests lower them.
	peekTimeout time.Duration
	maxConns    int
	// upstreamPort is the port an intercepted host's upstream is dialled on: 443, which tests
	// change to their TLS server's.
	upstreamPort uint16
	dialer       *net.Dialer
	forward      *http.Transport // plain HTTP to ordinary hosts
	upstream     *http.Transport // TLS to the intercepted hosts
	dispatcher   *http.Transport // Config.Dispatcher only, without the egress check
	srv          *http.Server
	inner        *http.Server // the requests inside intercepted tunnels
	queue        *connQueue   // inner's listener

	ctx    context.Context // every request's base; Close cancels it
	cancel context.CancelFunc

	mu        sync.Mutex
	closed    bool // no new connection, handler or copy
	done      bool // Close returned: nothing is recorded
	serving   bool
	ln        net.Listener
	conns     map[*trackedConn]struct{}
	nclients  int            // tracked client connections
	wg        sync.WaitGroup // handlers and copy goroutines
	closeOnce sync.Once
}

// New validates cfg and builds a gateway; it starts nothing.
func New(cfg Config) (*Gateway, error) {
	switch {
	case cfg.TaskID == "":
		return nil, &ConfigError{Field: "TaskID"}
	case cfg.RunID == "":
		return nil, &ConfigError{Field: "RunID"}
	case len(cfg.Clients) == 0:
		return nil, &ConfigError{Field: "Clients"}
	case cfg.Sink == nil:
		return nil, &ConfigError{Field: "Sink"}
	case cfg.CA == nil:
		return nil, &ConfigError{Field: "CA"}
	case !validDispatcher(cfg.Dispatcher):
		return nil, &ConfigError{Field: "Dispatcher"}
	case cfg.DispatcherToken == nil:
		return nil, &ConfigError{Field: "DispatcherToken"}
	}
	clients := map[netip.Addr]bool{}
	for i, a := range cfg.Clients {
		if !a.IsValid() || a.IsUnspecified() || a.Zone() != "" {
			return nil, &ConfigError{Field: fmt.Sprintf("Clients[%d]", i)}
		}
		clients[a.Unmap()] = true
	}
	if err := cfg.Egress.Validate(); err != nil {
		if ee, ok := errors.AsType[*policy.EgressError](err); ok {
			return nil, &ConfigError{Field: "Egress." + ee.Field, Err: ee.Err}
		}
		return nil, &ConfigError{Field: "Egress"}
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	g := &Gateway{
		cfg: cfg, clients: clients, clk: cfg.Clock,
		log:   cfg.Logger.With("task", cfg.TaskID, "run", cfg.RunID),
		conns: map[*trackedConn]struct{}{}, peekTimeout: peekTimeout, maxConns: maxConns,
		upstreamPort: policy.InterceptPort, queue: newConnQueue(),
	}
	g.ifaces = &hostAddrs{read: net.InterfaceAddrs, clk: cfg.Clock, log: g.log}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.dialer = &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive, Control: g.controlDial}
	g.forward = &http.Transport{
		Proxy: nil, DialContext: g.dialEgress, ForceAttemptHTTP2: false, DisableCompression: true,
		ResponseHeaderTimeout: responseHeaderTimeout, IdleConnTimeout: idleTimeout,
		MaxIdleConnsPerHost: 16,
	}
	// HTTP/1.1 only (a custom TLS config and no ForceAttemptHTTP2), so an upgrade is never sent
	// over h2.
	g.upstream = &http.Transport{
		Proxy: nil, DialContext: g.dialEgress, ForceAttemptHTTP2: false, DisableCompression: true,
		TLSClientConfig:     &tls.Config{RootCAs: cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: tlsHandshakeTimeout, ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout: idleTimeout, MaxIdleConnsPerHost: 16,
	}
	g.dispatcher = g.newDispatcherTransport()
	g.srv = &http.Server{
		Handler: http.HandlerFunc(g.handle), ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes: maxHeaderBytes, IdleTimeout: idleTimeout,
		BaseContext: func(net.Listener) context.Context { return g.ctx },
		ErrorLog:    log.New(discard{}, "", 0),
	}
	g.inner = &http.Server{
		Handler: http.HandlerFunc(g.interceptRequest), ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes: maxHeaderBytes, IdleTimeout: idleTimeout,
		BaseContext: func(net.Listener) context.Context { return g.ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if ic, ok := c.(*interceptConn); ok {
				return context.WithValue(ctx, decisionKey{}, ic.d)
			}
			return ctx
		},
		DisableGeneralOptionsHandler: true, // OPTIONS * reaches the handler: not-proxy
		ErrorLog:                     log.New(discard{}, "", 0),
	}
	return g, nil
}

// validDispatcher: an absolute http or https URL with a host and without user info, query or
// fragment.
func validDispatcher(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

// Serve accepts the VM's connections on ln until ctx ends or Close is called, and returns nil
// then; it closes ln. It refuses a Clients address that is one of the agent host's own interface
// addresses (a *ConfigError), and a second call.
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error {
	own, err := g.ifaces.addrs(true)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("gateway: the agent host's addresses: %w", err)
	}
	for i, a := range g.cfg.Clients {
		if own[a.Unmap()] {
			_ = ln.Close()
			return &ConfigError{Field: fmt.Sprintf("Clients[%d]", i)}
		}
	}
	g.mu.Lock()
	switch {
	case g.closed:
		g.mu.Unlock()
		_ = ln.Close()
		return nil
	case g.serving:
		g.mu.Unlock()
		_ = ln.Close()
		return errors.New("gateway: Serve called twice")
	}
	g.serving = true
	g.ln = &listener{Listener: ln, g: g}
	g.mu.Unlock()

	if g.hold() {
		go func() {
			defer g.wg.Done()
			_ = g.inner.Serve(g.queue) // until Close
		}()
	}
	stop := context.AfterFunc(ctx, func() { _ = g.Close() })
	defer stop()
	err = g.srv.Serve(g.ln)
	if g.isClosed() {
		_ = g.Close() // wait for a Close that ctx started
		return nil
	}
	return fmt.Errorf("gateway: serve: %w", err)
}

func (g *Gateway) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// Close stops the gateway: it closes the listener and every connection (hijacked ones, tunnels'
// upstream sides included), cancels the requests in flight, waits for every handler and copy
// (whose records are written then), and returns nil.
// Nothing is recorded after it returns. It is idempotent; a concurrent call waits for the first.
func (g *Gateway) Close() error {
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closed = true
		ln := g.ln
		conns := make([]*trackedConn, 0, len(g.conns))
		for c := range g.conns {
			conns = append(conns, c)
		}
		g.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		for _, c := range conns {
			_ = c.Close()
		}
		g.cancel()
		_ = g.srv.Close()
		_ = g.inner.Close()
		_ = g.queue.Close()
		g.wg.Wait()
		g.forward.CloseIdleConnections()
		g.upstream.CloseIdleConnections()
		g.dispatcher.CloseIdleConnections()

		g.mu.Lock()
		g.done = true
		g.mu.Unlock()
	})
	return nil
}

// hold counts a handler or copy goroutine in wg; false once the gateway is closed.
func (g *Gateway) hold() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	return true
}

// record sends r to the Sink unless Close has returned. The LLD's 50,000-record cap is
// deferred (PoC).
func (g *Gateway) record(r Record) {
	r.Type = RecordType
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.done {
		g.cfg.Sink.Egress(r)
	}
}

// listener admits only the VM's connections and tracks them.
type listener struct {
	net.Listener
	g *Gateway
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if tc := l.g.admit(c); tc != nil {
			return tc, nil
		}
	}
}

// admit tracks c when it comes from a Clients address, the gateway is open and under g.maxConns;
// otherwise it closes c (a peer outside Clients with a record) and returns nil.
func (g *Gateway) admit(c net.Conn) *trackedConn {
	start := g.clk.Now()
	if !g.allowedPeer(c.RemoteAddr()) {
		_ = c.Close()
		g.log.Info("gateway: refused a connection", "refusal", policy.CodePeer)
		g.record(Record{At: start, Mode: ModeAccept, Refusal: policy.CodePeer})
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.nclients >= g.maxConns {
		_ = c.Close()
		if !g.closed {
			g.log.Info("gateway: connection limit reached; closed a connection", "limit", g.maxConns)
		}
		return nil
	}
	tc := &trackedConn{Conn: c, g: g, client: true}
	g.conns[tc] = struct{}{}
	g.nclients++
	return tc
}

// allowedPeer reports whether a is exactly one of Clients (an IPv4-mapped address unmapped).
func (g *Gateway) allowedPeer(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok || ta.Zone != "" {
		return false
	}
	return g.clients[ip.Unmap()]
}

// track wraps an upstream connection so Close closes it; nil and closed when the gateway is.
func (g *Gateway) track(c net.Conn) *trackedConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		_ = c.Close()
		return nil
	}
	tc := &trackedConn{Conn: c, g: g}
	g.conns[tc] = struct{}{}
	return tc
}

// trackedConn is a connection Close closes: every accepted one, from accept until it closes
// (hijacked or not), and every upstream one.
type trackedConn struct {
	net.Conn
	g      *Gateway
	client bool
	once   sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.g.mu.Lock()
		delete(c.g.conns, c)
		if c.client {
			c.g.nclients--
		}
		c.g.mu.Unlock()
	})
	return err
}

// CloseWrite half-closes a TCP connection; it does nothing for another kind.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
