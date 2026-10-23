package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/ca"
	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/clock"
)

// testCA is a new gateway CA on the real clock: Go's TLS verifies leaves on it.
func testCA(t *testing.T) *ca.CA {
	t.Helper()
	c, err := ca.Generate(context.Background(), filepath.Join(t.TempDir(), "gateway-ca"), clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// seen is what the upstream received.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
}

func (s *seen) all() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

// startIntercept serves a gateway whose intercepted hosts all resolve to one TLS upstream
// (a certificate for each, from a CA of its own) running h after it records the request.
func startIntercept(t *testing.T, h http.HandlerFunc, mut func(*Config)) (*env, *seen) {
	t.Helper()
	upCA := testCA(t)
	got := &seen{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.add(r.Clone(context.Background()))
		h(w, r)
	}))
	srv.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return upCA.Leaf(h.ServerName)
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	e := start(t, func(c *Config) {
		c.UpstreamRoots = upCA.Pool()
		c.Egress.OpenAddrs = []netip.AddrPort{ap}
		if mut != nil {
			mut(c)
		}
	}, func(g *Gateway) { g.upstreamPort = ap.Port() })
	for _, host := range policy.InterceptedHosts() {
		e.res.set(host, addrs("127.0.0.1"))
	}
	return e, got
}

// readConn reads what a reply's reader buffered first.
type readConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// dialTLS opens an intercepted tunnel to host and handshakes with sni, trusting only the
// gateway's CA.
func (h *env) dialTLS(t *testing.T, host, sni string) (*tls.Conn, error) {
	t.Helper()
	rp := h.send(t, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n")
	if rp.status != http.StatusOK {
		t.Fatalf("CONNECT %s: %d %s", host, rp.status, rp.code)
	}
	tc := tls.Client(&readConn{Conn: rp.conn, r: rp.br}, &tls.Config{RootCAs: h.g.cfg.CA.Pool(),
		ServerName: sni, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	return tc, tc.Handshake()
}

// roundTrip writes raw on c and reads the response: its status, header and body.
func roundTrip(t *testing.T, c net.Conn, br *bufio.Reader, raw string) (int, http.Header, string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// TestIntercept: every intercepted host's leaf verifies, under deny-by-default too, and each
// request reaches its host's upstream with every carrier stripped and the class's cookie rule.
func TestIntercept(t *testing.T) {
	e, got := startIntercept(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}, func(c *Config) { c.Egress.Mode = policy.ModeDenyByDefault })
	for i, host := range policy.InterceptedHosts() {
		t.Run(host, func(t *testing.T) {
			tc, err := e.dialTLS(t, host, host)
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			hostHeader := strings.ToUpper(host) + ".:443" // normalised to the CONNECT's host
			status, _, body := roundTrip(t, tc, bufio.NewReader(tc), "GET /p/x?q=query-secret HTTP/1.1\r\nHost: "+hostHeader+
				"\r\nAuthorization: Bearer sk-live-secret-token\r\nX-Api-Key: header-secret\r\n"+
				"Proxy-Authorization: Basic header-secret\r\nCookie: __oailb=lb; session=header-secret\r\n\r\n")
			if status != http.StatusOK || body != "ok" {
				t.Fatalf("got %d %q", status, body)
			}
			r := got.all()[i]
			if r.Host != host || r.URL.Path != "/p/x" || r.URL.RawQuery != "q=query-secret" {
				t.Errorf("upstream got Host %q, %s", r.Host, r.URL)
			}
			for _, k := range policy.CarrierHeaders() {
				if v := r.Header.Get(k); v != "" {
					t.Errorf("upstream got %s: %q", k, v)
				}
			}
			wantCookie := map[policy.CookieRule]string{policy.CookiesInfraOnly: "__oailb=lb", policy.CookiesStripped: ""}
			if c := r.Header.Get("Cookie"); c != wantCookie[policy.ClassOf(host).Cookies()] {
				t.Errorf("upstream got Cookie %q", c)
			}
			rs := e.sink.wait(t, 2*(i+1))
			conn, rec := rs[2*i], rs[2*i+1]
			if conn.Mode != ModeConnect || conn.Status != http.StatusOK || conn.Host != host {
				t.Errorf("CONNECT record %+v", conn)
			}
			if rec.Mode != ModeIntercept || rec.Status != http.StatusOK || rec.Host != host ||
				rec.Port != 443 || rec.Path != "/p/x" || rec.Down != 2 || rec.Refusal != "" {
				t.Errorf("request record %+v", rec)
			}
		})
	}
}

func TestInterceptRefusals(t *testing.T) {
	e, got := startIntercept(t, func(http.ResponseWriter, *http.Request) {}, nil)
	tests := []struct {
		name, raw string
		code      policy.Code
	}{
		{"CONNECT inside", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n", policy.CodeNotProxy},
		{"absolute form", "GET https://github.com/x HTTP/1.1\r\nHost: github.com\r\n\r\n", policy.CodeNotProxy},
		{"OPTIONS *", "OPTIONS * HTTP/1.1\r\nHost: github.com\r\n\r\n", policy.CodeNotProxy},
		{"another port", "GET / HTTP/1.1\r\nHost: github.com:8443\r\n\r\n", policy.CodeHostMismatch},
		{"another host", "GET / HTTP/1.1\r\nHost: api.github.com\r\n\r\n", policy.CodeHostMismatch},
		{"an IP", "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", policy.CodeHostMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := e.dialTLS(t, "github.com", "github.com")
			if err != nil {
				t.Fatal(err)
			}
			status, h, body := roundTrip(t, tc, bufio.NewReader(tc), tt.raw)
			want, _ := tt.code.Response("")
			if status != want.Status || policy.Code(h.Get(policy.RefusalHeader)) != tt.code || body != want.Body {
				t.Errorf("got %d %q, want %d %s", status, body, want.Status, tt.code)
			}
			r := last(t, e.sink.wait(t, 1))
			if r.Mode != ModeIntercept || r.Refusal != tt.code {
				t.Errorf("record %+v, want intercept/%s", r, tt.code)
			}
		})
	}

	t.Run("SNI mismatch", func(t *testing.T) {
		if _, err := e.dialTLS(t, "github.com", "evil.example.com"); err == nil {
			t.Fatal("the handshake succeeded for another SNI")
		}
		deadline := time.Now().Add(5 * time.Second)
		for r := last(t, e.sink.all()); r.Refusal != policy.CodeSNIMismatch; r = last(t, e.sink.all()) {
			if time.Now().After(deadline) {
				t.Fatalf("last record %+v, want connect/sni-mismatch", r)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	if n := len(got.all()); n != 0 {
		t.Errorf("a refused request reached the upstream %d times", n)
	}
}

// TestInterceptWebSocket: an upgrade passes through with its carriers stripped, its bytes are
// copied both ways, and Close ends it with its record written.
func TestInterceptWebSocket(t *testing.T) {
	e, got := startIntercept(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(c, brw)
	}, nil)
	tc, err := e.dialTLS(t, "chatgpt.com", "chatgpt.com")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(tc)
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(tc, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer sk-live-secret-token\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // a 101 has no body
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(tc, "frame")
	b := make([]byte, 5)
	if _, err := io.ReadFull(br, b); err != nil || string(b) != "frame" {
		t.Fatalf("echo %q, %v", b, err)
	}
	if a := got.all()[0].Header.Get("Authorization"); a != "" {
		t.Errorf("upstream got Authorization %q", a)
	}

	began := time.Now()
	e.close(t)
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("Close took %v", d)
	}
	_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(br); err != nil {
		if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
			t.Error("the upgraded connection is still open after Close")
		}
	}
	r := last(t, e.sink.all())
	if r.Mode != ModeIntercept || r.Status != http.StatusSwitchingProtocols || r.Path != "/backend-api/codex/responses" {
		t.Errorf("record %+v, want the upgrade's with 101", r)
	}
}

// TestInterceptUpstreamUnverified: an upstream whose certificate the roots don't trust is
// upstream (502), never forwarded in the clear.
func TestInterceptUpstreamUnverified(t *testing.T) {
	e, got := startIntercept(t, func(http.ResponseWriter, *http.Request) {}, func(c *Config) {
		c.UpstreamRoots = testCA(t).Pool()
	})
	tc, err := e.dialTLS(t, "api.github.com", "api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	status, h, _ := roundTrip(t, tc, bufio.NewReader(tc), "GET /repos HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	if c := policy.Code(h.Get(policy.RefusalHeader)); status != http.StatusBadGateway || c != policy.CodeUpstream {
		t.Errorf("got %d %s, want 502 upstream", status, c)
	}
	if n := len(got.all()); n != 0 {
		t.Errorf("the upstream was reached %d times", n)
	}
}

// TestInterceptAbortedBody: a response body cut short after its header went out (ReverseProxy
// aborts the handler) still leaves the request's record.
func TestInterceptAbortedBody(t *testing.T) {
	e, _ := startIntercept(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "ab")
		c, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}, nil)
	tc, err := e.dialTLS(t, "claude.ai", "claude.ai")
	if err != nil {
		t.Fatal(err)
	}
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(tc, "GET /cut HTTP/1.1\r\nHost: claude.ai\r\n\r\n")
	_, _ = io.ReadAll(tc)
	r := last(t, e.sink.wait(t, 2))
	if r.Mode != ModeIntercept || r.Status != http.StatusOK || r.Path != "/cut" {
		t.Errorf("record %+v, want the aborted request's", r)
	}
}
