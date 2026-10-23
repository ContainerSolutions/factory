package gateway

import (
	"bufio"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
)

// TestTunnelSNI runs real TLS clients through tunnels to an httptest TLS server, whose
// certificate names example.com (resolved to 127.0.0.1 by the fake resolver).
func TestTunnelSNI(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through")
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused tunnels' aborted handshakes
	ts.StartTLS()
	t.Cleanup(ts.Close)
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	roots := ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	e := start(t, open(ts.Listener.Addr().String()))
	e.res.set("example.com", addrs("127.0.0.1"))
	tests := []struct {
		name, connect, sni string
		refusal            policy.Code
	}{
		{"matching SNI", "example.com", "example.com", ""},
		{"matching SNI, other case", "example.com", "EXAMPLE.com", ""},
		{"another SNI", "example.com", "other.test", policy.CodeSNIMismatch},
		{"no SNI", "example.com", "", ""},
		{"IP literal with a name", "127.0.0.1", "example.com", policy.CodeSNIMismatch},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := e.send(t, "CONNECT "+tt.connect+":"+port+" HTTP/1.1\r\nHost: x\r\n\r\n")
			if rp.status != http.StatusOK {
				t.Fatalf("CONNECT: %d %s", rp.status, rp.code)
			}
			cfg := &tls.Config{RootCAs: roots, ServerName: tt.sni, MinVersion: tls.VersionTLS12}
			if tt.sni == "" {
				cfg.InsecureSkipVerify = true //nolint:gosec // no SNI means no name to verify; the test checks the tunnel
			}
			tc := tls.Client(&bufConnPlain{Conn: rp.conn, r: rp.br}, cfg)
			err := tc.Handshake()
			if tt.refusal != "" {
				if err == nil {
					t.Fatal("the handshake went through a refused tunnel")
				}
			} else {
				if err != nil {
					t.Fatalf("handshake: %v", err)
				}
				_, _ = io.WriteString(tc, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
				resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if string(b) != "through" {
					t.Fatalf("body %q", b)
				}
			}
			_ = tc.Close()
			r := e.sink.wait(t, i+1)[i]
			if r.Refusal != tt.refusal || r.Status != http.StatusOK || r.Mode != ModeTunnel {
				t.Errorf("record %+v, want refusal %q after the 200", r, tt.refusal)
			}
		})
	}
}

// bufConnPlain reads a client connection's bytes already buffered by its response reader.
type bufConnPlain struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConnPlain) Read(b []byte) (int, error) { return c.r.Read(b) }

// TestPeekHello drives peekHello over a pipe: the non-TLS cases, the split, oversized and
// malformed hellos, and the timeout.
func TestPeekHello(t *testing.T) {
	hello := clientHello(t, "a.example.com")
	tests := []struct {
		name   string
		mode   string
		port   uint16
		writes []string
		want   policy.Code
		passed string // what peekHello returns to send upstream first ("" when nothing)
	}{
		{"hello", "", 443, []string{hello}, "", hello},
		{"hello split across writes", "", 443, []string{hello[:3], hello[3:40], hello[40:]}, "", hello},
		{"not TLS", "", 443, []string{"SSH-2.0-x\r\n"}, "", ""},
		{"not TLS, deny-by-default, 443", policy.ModeDenyByDefault, 443, []string{"GET / HTTP/1.1\r\n"}, policy.CodeNotTLS, ""},
		{"not TLS, deny-by-default, 22", policy.ModeDenyByDefault, 22, []string{"SSH-2.0-x\r\n"}, "", ""},
		{"nothing in time", "", 443, nil, "", ""},
		{"nothing in time, deny-by-default, 443", policy.ModeDenyByDefault, 443, nil, policy.CodeNotTLS, ""},
		{"malformed handshake record", "", 443, []string{"\x16\x03\x01\x00\x05hello"}, policy.CodeSNIMismatch, ""},
		{"over 16 KiB", "", 443, []string{"\x16\x03\x01\xff\xff" + strings.Repeat("x", peekMax)}, policy.CodeSNIMismatch, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := New(Config{TaskID: "t", RunID: "r", Clients: addrs("192.168.64.2"), Sink: &sink{t: t},
				Egress: policy.Egress{Mode: tt.mode, AllowHosts: []string{"*.example.com"}}})
			if err != nil {
				t.Fatal(err)
			}
			g.peekTimeout = 200 * time.Millisecond
			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
			go func() {
				for _, w := range tt.writes {
					if _, err := io.WriteString(client, w); err != nil {
						return
					}
				}
				_, _ = io.Copy(io.Discard, client) // never answer, never close
			}()
			bc := &bufConn{trackedConn: &trackedConn{Conn: server, g: g}, r: bufio.NewReader(server)}
			got, code := g.peekHello(bc, policy.Target{Host: "a.example.com", Port: tt.port})
			if code != tt.want || string(got) != tt.passed {
				t.Fatalf("peekHello = %d bytes, %q; want %d bytes, %q", len(got), code, len(tt.passed), tt.want)
			}
			if tt.want == "" && len(tt.writes) > 0 && tt.passed == "" {
				// A non-TLS stream stays buffered, whole, for the tunnel.
				b := make([]byte, len(tt.writes[0]))
				if _, err := io.ReadFull(bc, b); err != nil || string(b) != tt.writes[0] {
					t.Errorf("after the peek the tunnel reads %q, %v", b, err)
				}
			}
		})
	}
}

// clientHello is the first flight a Go TLS client sends for sni.
func clientHello(t *testing.T, sni string) string {
	t.Helper()
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	go func() {
		_ = tls.Client(client, &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS12}).Handshake()
	}()
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(server)
	head := make([]byte, 5)
	if _, err := io.ReadFull(br, head); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(head[3])<<8|int(head[4]))
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	return string(head) + string(body)
}

// TestDenyByDefault: only AllowHosts pass the host check, exact and wildcard entries with and
// without a port; IP literals never match; the gateway's own hosts aren't subject to it.
func TestDenyByDefault(t *testing.T) {
	echo := newEcho(t)
	p := echo.port()
	e := start(t, func(c *Config) {
		open(echo.addr())(c)
		c.Egress.Mode = policy.ModeDenyByDefault
		c.Egress.AllowHosts = []string{"*.allowed.test", "exact.test:" + p, "other.test:1"}
	})
	for _, n := range []string{"a.allowed.test", "a.b.allowed.test", "exact.test", "allowed.test", "other.test", "nope.test"} {
		e.res.set(n, addrs("127.0.0.1"))
	}
	tests := []struct {
		raw  string
		want policy.Code // "" for a tunnel or a forward that reached the echo
	}{
		{"CONNECT a.allowed.test:" + p, ""},
		{"CONNECT a.b.allowed.test:" + p, ""},
		{"CONNECT exact.test:" + p, ""},
		{"CONNECT allowed.test:" + p, policy.CodeEgressNotAllowed}, // the apex isn't under *.
		{"CONNECT other.test:" + p, policy.CodeEgressNotAllowed},   // another port
		{"CONNECT nope.test:" + p, policy.CodeEgressNotAllowed},
		{"CONNECT 127.0.0.1:" + p, policy.CodeEgressNotAllowed}, // an IP literal never matches
		{"GET http://nope.test:" + p + "/", policy.CodeEgressNotAllowed},
		{"CONNECT api.anthropic.com:443", policy.CodeUpstream}, // its own route: interception is PR 5's
	}
	for _, tt := range tests {
		t.Run(strings.ReplaceAll(tt.raw, p, "PORT"), func(t *testing.T) {
			rp := e.send(t, tt.raw+" HTTP/1.1\r\nHost: x\r\n\r\n")
			if tt.want == "" {
				if rp.status != http.StatusOK {
					t.Fatalf("got %d %s, want a tunnel", rp.status, rp.code)
				}
				// Non-TLS bytes to a port other than 443 pass the peek.
				_, _ = io.WriteString(rp.conn, "ping")
				b := make([]byte, 4)
				if _, err := io.ReadFull(rp.br, b); err != nil || string(b) != "ping" {
					t.Fatalf("echo %q, %v", b, err)
				}
				return
			}
			if rp.code != tt.want {
				t.Fatalf("got %d %s, want %s", rp.status, rp.code, tt.want)
			}
		})
	}
}
