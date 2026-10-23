package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
)

// tlsHandshakeTimeout bounds each side's TLS handshake.
const tlsHandshakeTimeout = 10 * time.Second

// errSNIMismatch fails a handshake whose SNI isn't the CONNECT's host.
var errSNIMismatch = errors.New("gateway: the SNI does not match the CONNECT")

// intercept terminates the client's TLS on an intercepted CONNECT with the CA's leaf for t's
// host (TLS 1.2 or later, ALPN http/1.1 only) and hands the session to the inner server, which
// serves its requests (interceptRequest). It records the CONNECT once the handshake is over: a
// handshake whose SNI is another host is closed with sni-mismatch.
func (g *Gateway) intercept(client *bufConn, d policy.Decision, rec Record) {
	host := d.Target.Host
	mismatch := false
	tc := tls.Server(client, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if h.ServerName != "" && policy.NormalizeHost(h.ServerName) != host {
				mismatch = true
				return nil, errSNIMismatch
			}
			return g.cfg.CA.Leaf(host)
		},
	})
	ctx, cancel := context.WithTimeout(g.ctx, tlsHandshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		g.log.Debug("gateway: client TLS handshake failed", "err", err)
		if mismatch {
			rec.Refusal = policy.CodeSNIMismatch
		}
		_ = tc.Close()
		g.finish(rec)
		return
	}
	g.finish(rec)
	if !g.queue.push(&interceptConn{Conn: tc, d: d}) {
		_ = tc.Close()
	}
}

// interceptConn is a terminated TLS session and the CONNECT it came from.
type interceptConn struct {
	net.Conn
	d policy.Decision
}

type decisionKey struct{}

// interceptRequest serves one request inside an intercepted tunnel: origin form only
// (not-proxy), its Host normalised and checked against the CONNECT (host-mismatch), every
// credential carrier stripped and the class's cookie rule applied, the class's route rule and
// credentials (routeCredentials), then forwarded to the CONNECT's host over TLS through the
// egress dialer. A WebSocket upgrade passes through, with the same credentials.
func (g *Gateway) interceptRequest(w http.ResponseWriter, r *http.Request) {
	if !g.hold() {
		writeRefusal(w, policy.CodeClosing, "")
		return
	}
	defer g.wg.Done()
	start := g.clk.Now()
	d, _ := r.Context().Value(decisionKey{}).(policy.Decision)
	rec := g.newRecord(r, ModeIntercept, start)
	rec.Host, rec.Port = d.Target.Host, d.Target.Port
	// Deferred: ReverseProxy panics with http.ErrAbortHandler when the response body's copy
	// fails after the header went out, and the request is still recorded then.
	defer func() { g.finish(rec) }()
	switch {
	case r.Method == http.MethodConnect || r.URL.IsAbs() || r.URL.Host != "" || r.RequestURI == "*":
		rec.Path = ""
		rec.Status, rec.Refusal = writeRefusal(w, policy.CodeNotProxy, "")
	case policy.CheckHost(r.Host, d.Target.Host) != "":
		rec.Status, rec.Refusal = writeRefusal(w, policy.CodeHostMismatch, "")
	default:
		if set, ok := g.routeCredentials(w, r, d, &rec); ok {
			g.forwardIntercepted(w, r, d, &rec, set)
		}
	}
}

// forwardIntercepted sends r to https://<the CONNECT's host> through g.upstream, with every
// carrier stripped, the class's cookie rule applied and then set's headers (the credentials).
func (g *Gateway) forwardIntercepted(w http.ResponseWriter, r *http.Request, d policy.Decision, rec *Record, set http.Header) {
	host := net.JoinHostPort(d.Target.Host, strconv.Itoa(int(g.upstreamPort)))
	g.proxy(w, r, rec, g.upstream, func(out *http.Request) {
		out.URL = &url.URL{Scheme: "https", Host: host, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		out.Host = d.Target.Host
		stripCarriers(out, d.Class)
		for k, vs := range set {
			out.Header[k] = vs
		}
	})
}

// proxy sends r through tr after rewrite sets its outbound URL and headers. A WebSocket
// upgrade passes through. rec gets the status and byte counts on every exit, the ReverseProxy's
// http.ErrAbortHandler panic included, and the refusal of an upstream error.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, rec *Record, tr http.RoundTripper, rewrite func(out *http.Request)) {
	up := &countReader{r: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = up
	}
	cw := &countWriter{ResponseWriter: w}
	status := 0
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite:   func(pr *httputil.ProxyRequest) { rewrite(pr.Out) },
		ModifyResponse: func(res *http.Response) error {
			status = res.StatusCode // a 101's header is written past cw, on the hijacked conn
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			code, class := g.refusalOf(err)
			rec.Status, rec.Refusal = writeRefusal(w, code, class)
		},
		ErrorLog: g.srv.ErrorLog,
	}
	defer func() { // also when ServeHTTP panics with http.ErrAbortHandler
		if rec.Refusal == "" {
			rec.Status = cw.status()
			if status == http.StatusSwitchingProtocols {
				rec.Status = status
			}
		}
		rec.Up, rec.Down = up.n.Load(), cw.n
	}()
	rp.ServeHTTP(cw, r)
}

// stripCarriers removes every credential carrier from out (the VM's placeholders are replaced,
// never compared) and applies the class's cookie rule.
func stripCarriers(out *http.Request, c policy.Class) {
	if c.StripsCarriers() {
		for _, h := range policy.CarrierHeaders() {
			out.Header.Del(h)
		}
		out.URL.User = nil
	}
	switch c.Cookies() {
	case policy.CookiesStripped:
		out.Header.Del("Cookie")
	case policy.CookiesInfraOnly:
		if kept := policy.FilterCookies(out.Header.Values("Cookie")); kept != nil {
			out.Header["Cookie"] = kept
		} else {
			out.Header.Del("Cookie")
		}
	case policy.CookiesUntouched:
	}
}

// connQueue is the inner server's listener: intercept pushes each terminated session to it.
type connQueue struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn), done: make(chan struct{})}
}

// push hands c to the inner server; false once the queue is closed.
func (q *connQueue) push(c net.Conn) bool {
	select {
	case q.ch <- c:
		return true
	case <-q.done:
		return false
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	q.once.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{} }
