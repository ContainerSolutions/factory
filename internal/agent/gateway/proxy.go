package gateway

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
)

// handle is the proxy's handler: CONNECT, and every other request as plain HTTP.
func (g *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	if !g.hold() {
		writeRefusal(w, policy.CodeClosing, "")
		return
	}
	defer g.wg.Done()
	start := g.clk.Now()
	if r.Method == http.MethodConnect {
		g.connect(w, r, start)
		return
	}
	g.plain(w, r, start)
}

// plain serves an absolute-form http request: the host table, then forwarding through the
// egress dialer. An origin-form request, and any scheme but http, is not-proxy.
func (g *Gateway) plain(w http.ResponseWriter, r *http.Request, start time.Time) {
	rec := g.newRecord(r, ModeForward, start)
	// Deferred: a response body cut after its headers went out makes the ReverseProxy panic
	// with http.ErrAbortHandler, and the request is still one record.
	defer func() { g.finish(rec) }()
	d := policy.DecidePlain(r.URL.Scheme, r.URL.Host, g.cfg.Egress)
	rec.Host, rec.Port = d.Target.Host, d.Target.Port
	switch d.Action {
	case policy.Forward:
		g.forwardPlain(w, r, &rec)
	case policy.ToDispatcher:
		// The dispatcher route and its transport are PR 7's: until then the route is closed.
		rec.Mode = ModeDispatcher
		rec.Status, rec.Refusal = writeRefusal(w, policy.CodeDispatcherRoute, "")
	default:
		rec.Status, rec.Refusal = writeRefusal(w, d.Refusal, "")
	}
}

// forwardPlain sends r to its own host through the egress dialer. Only Proxy-Authorization is
// dropped: an ordinary host gets no credential and keeps the rest. rec gets its status and byte
// counts on every exit, the ReverseProxy's http.ErrAbortHandler panic included.
func (g *Gateway) forwardPlain(w http.ResponseWriter, r *http.Request, rec *Record) {
	up := &countReader{r: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = up
	}
	cw := &countWriter{ResponseWriter: w}
	refused := false
	rp := &httputil.ReverseProxy{
		Transport: g.forward,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: pr.In.URL.Host, User: pr.In.URL.User,
				Path: pr.In.URL.Path, RawPath: pr.In.URL.RawPath, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = ""
			pr.Out.Header.Del("Proxy-Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			refused = true
			code, class := g.refusalOf(err)
			rec.Status, rec.Refusal = writeRefusal(w, code, class)
		},
		ErrorLog: g.srv.ErrorLog,
	}
	defer func() {
		if !refused {
			rec.Status = cw.status()
		}
		rec.Up, rec.Down = up.n.Load(), cw.n
	}()
	rp.ServeHTTP(cw, r)
}

// connect serves a CONNECT: the host table, then a tunnel through the egress dialer. A refused
// CONNECT gets its answer and the connection is closed.
func (g *Gateway) connect(w http.ResponseWriter, r *http.Request, start time.Time) {
	d := policy.DecideConnect(r.RequestURI, g.cfg.Egress)
	mode := ModeTunnel
	if d.Class != policy.Ordinary {
		mode = ModeConnect
	}
	rec := g.newRecord(r, mode, start)
	rec.Host, rec.Port = d.Target.Host, d.Target.Port
	refuse := func(code policy.Code, class policy.DenyClass) {
		w.Header().Set("Connection", "close")
		rec.Status, rec.Refusal = writeRefusal(w, code, class)
		g.finish(rec)
	}
	var upConn net.Conn
	switch d.Action {
	case policy.Intercept: // TLS-terminated after the 200 (intercept)
	case policy.Tunnel:
		c, err := g.dialEgress(r.Context(), "tcp", d.Target.String())
		if err != nil {
			refuse(g.refusalOf(err))
			return
		}
		upConn = c
	default:
		refuse(d.Refusal, "")
		return
	}
	closeUp := func() {
		if upConn != nil {
			_ = upConn.Close()
		}
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		closeUp()
		refuse(policy.CodeUpstream, "")
		return
	}
	tc, tracked := conn.(*trackedConn) // every accepted connection is one
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || !tracked {
		_ = conn.Close()
		closeUp()
		g.finish(rec) // status 0: the client went away
		return
	}
	rec.Status = http.StatusOK
	client := &bufConn{trackedConn: tc, r: brw.Reader}
	if d.Action == policy.Intercept {
		g.intercept(client, d, rec) // the inner server owns the connection from here
		return
	}
	defer func() { _ = conn.Close() }()
	defer closeUp()
	rec.Up, rec.Down, rec.Refusal = g.splice(client, upConn, d.Target)
	g.finish(rec)
}

// splice copies both ways until both directions end, and returns the bytes from the client and
// to it, and the peek's refusal. Server bytes flow from the start, so a protocol where the
// server speaks first isn't held up by the peek; no client byte goes upstream before it passes.
// An EOF half-closes the other side; an error closes both, so the other copy ends too.
func (g *Gateway) splice(client *bufConn, up net.Conn, t policy.Target) (upN, downN int64, refusal policy.Code) {
	closeBoth := func() { _ = client.Close(); _ = up.Close() }
	copyHalf := func(dst, src net.Conn) int64 {
		n, err := io.Copy(dst, src)
		if err != nil {
			closeBoth()
		} else if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		return n
	}
	if !g.hold() {
		closeBoth()
		return 0, 0, policy.CodeClosing
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer wg.Done()
		downN = copyHalf(client, up)
	}()
	hello, refusal := g.peekHello(client, t)
	switch {
	case refusal != "":
		closeBoth()
	default:
		if _, err := up.Write(hello); err != nil {
			closeBoth()
			break
		}
		upN = int64(len(hello)) + copyHalf(up, client)
	}
	wg.Wait()
	closeBoth()
	return upN, downN, refusal
}

// errHelloRead aborts the peek's TLS server once it has the ClientHello.
var errHelloRead = errors.New("gateway: ClientHello read")

// peekHello reads the client's ClientHello (at most peekMax bytes, for g.peekTimeout) without
// answering it, and returns the bytes it consumed, to be sent upstream first, and a refusal:
// sni-mismatch for an SNI that isn't t's host, or a handshake record that doesn't parse within
// peekMax; not-tls under deny-by-default to port 443 when the first byte isn't a handshake
// record or nothing arrives in time. A ClientHello without an SNI, and other non-TLS bytes,
// pass. A tls.Server over the bytes parses it, so no parser is added.
func (g *Gateway) peekHello(c *bufConn, t policy.Target) ([]byte, policy.Code) {
	_ = c.SetReadDeadline(time.Now().Add(g.peekTimeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	if first, err := c.r.Peek(1); err != nil || first[0] != recordTypeHandshake {
		if g.cfg.Egress.Mode != "" && g.cfg.Egress.Mode != policy.ModeDefault && t.Port == policy.InterceptPort {
			return nil, policy.CodeNotTLS
		}
		return nil, "" // what was peeked stays buffered in c
	}
	var read bytes.Buffer
	var sni string
	seen := false
	hc := &helloConn{Conn: c, r: io.TeeReader(io.LimitReader(c, peekMax), &read)}
	_ = tls.Server(hc, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni, seen = h.ServerName, true
		return nil, errHelloRead
	}}).Handshake()
	switch {
	case !seen, sni != "" && policy.NormalizeHost(sni) != t.Host:
		return nil, policy.CodeSNIMismatch
	}
	return read.Bytes(), ""
}

const (
	peekMax             = 16 << 10 // a ClientHello's bytes
	peekTimeout         = 10 * time.Second
	recordTypeHandshake = 0x16 // a TLS handshake record's first byte
)

// helloConn replays the client's bytes to the peek's TLS server and refuses its writes, so the
// client never gets an answer from it.
type helloConn struct {
	net.Conn
	r io.Reader
}

func (h *helloConn) Read(b []byte) (int, error) { return h.r.Read(b) }

func (h *helloConn) Write([]byte) (int, error) { return 0, errHelloRead }

// writeRefusal answers code (and class, for egress-denied) with policy's response, and returns
// the status and code for the record.
func writeRefusal(w http.ResponseWriter, code policy.Code, class policy.DenyClass) (int, policy.Code) {
	resp, ok := code.Response(class)
	if !ok { // a code without an answer, or an unknown class: never the VM's text
		code = policy.CodeUpstream
		resp, _ = code.Response("")
	}
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(resp.Body)))
	w.WriteHeader(resp.Status)
	_, _ = io.WriteString(w, resp.Body)
	return resp.Status, code
}

// bufConn reads what the server had already buffered from a hijacked connection first.
type bufConn struct {
	*trackedConn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// countReader counts a request body's bytes.
type countReader struct {
	r io.ReadCloser
	n atomic.Int64 // the transport may still be reading when the handler records
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (c *countReader) Close() error { return c.r.Close() }

// countWriter keeps the status and counts the body bytes a handler sent.
type countWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (c *countWriter) WriteHeader(code int) {
	if c.code == 0 && code >= 200 || code == http.StatusSwitchingProtocols {
		c.code = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *countWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}
