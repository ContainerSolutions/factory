package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

func TestNewRefuses(t *testing.T) {
	ok := Config{TaskID: "t", RunID: "r", Clients: addrs("192.168.64.2"), Sink: &sink{t: t}, CA: testCA(t), Dispatcher: testDispatcher, DispatcherToken: noToken}
	tests := []struct {
		name, field string
		mut         func(*Config)
	}{
		{"no task", "TaskID", func(c *Config) { c.TaskID = "" }},
		{"no clients", "Clients", func(c *Config) { c.Clients = nil }},
		{"no CA", "CA", func(c *Config) { c.CA = nil }},
		{"unspecified client", "Clients[0]", func(c *Config) { c.Clients = addrs("0.0.0.0") }},
		{"bad allow host", "Egress.AllowHosts[0]", func(c *Config) { c.Egress.AllowHosts = []string{"localhost"} }},
		{"no dispatcher", "Dispatcher", func(c *Config) { c.Dispatcher = nil }},
		{"dispatcher with a query", "Dispatcher", func(c *Config) { c.Dispatcher = &url.URL{Scheme: "http", Host: "h", RawQuery: "a=b"} }},
		{"dispatcher with user info", "Dispatcher", func(c *Config) { c.Dispatcher = &url.URL{Scheme: "https", Host: "h", User: url.User("u")} }},
		{"dispatcher not http", "Dispatcher", func(c *Config) { c.Dispatcher = &url.URL{Scheme: "ftp", Host: "h"} }},
		{"no dispatcher token", "DispatcherToken", func(c *Config) { c.DispatcherToken = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.mut(&cfg)
			_, err := New(cfg)
			ce, isCE := errors.AsType[*ConfigError](err)
			if !isCE || ce.Field != tt.field || !errors.Is(err, ErrConfig) {
				t.Fatalf("New: %v, want a ConfigError for %s", err, tt.field)
			}
		})
	}
	if _, err := New(ok); err != nil {
		t.Fatalf("New(ok): %v", err)
	}
}

func TestClientPin(t *testing.T) {
	g, err := New(Config{TaskID: "t", RunID: "r", Clients: addrs("192.168.64.2"), Sink: &sink{t: t}, CA: testCA(t), Dispatcher: testDispatcher, DispatcherToken: noToken})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		peer net.Addr
		want bool
	}{
		{&net.TCPAddr{IP: net.ParseIP("192.168.64.2"), Port: 5000}, true},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:192.168.64.2"), Port: 5000}, true}, // 16-byte form
		{&net.TCPAddr{IP: net.ParseIP("192.168.64.3"), Port: 5000}, false},       // same /29
		{&net.TCPAddr{IP: net.ParseIP("192.168.64.1"), Port: 5000}, false},       // the host's own
		{&net.TCPAddr{IP: net.ParseIP("fe80::2"), Zone: "en0"}, false},
		{&net.UnixAddr{Name: "/tmp/x"}, false},
	} {
		if got := g.allowedPeer(tt.peer); got != tt.want {
			t.Errorf("allowedPeer(%v) = %v, want %v", tt.peer, got, tt.want)
		}
	}

	t.Run("the host's own address is refused by Serve", func(t *testing.T) {
		g, err := New(Config{TaskID: "t", RunID: "r", Clients: addrs("192.168.64.2", "192.168.64.1"), Sink: &sink{t: t}, CA: testCA(t), Dispatcher: testDispatcher, DispatcherToken: noToken})
		if err != nil {
			t.Fatal(err)
		}
		g.ifaces.read = hostIfaces
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		err = g.Serve(context.Background(), ln)
		if ce, ok := errors.AsType[*ConfigError](err); !ok || ce.Field != "Clients[1]" {
			t.Fatalf("Serve: %v, want a ConfigError for Clients[1]", err)
		}
	})

	t.Run("another peer is closed at accept with a record", func(t *testing.T) {
		e := start(t, func(c *Config) { c.Clients = addrs("127.0.0.2") })
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, "GET http://example.test/ HTTP/1.1\r\nHost: example.test\r\n\r\n")
		if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("read %d bytes, %v: the connection wasn't closed", n, err)
		}
		r := last(t, e.sink.wait(t, 1))
		if r.Mode != ModeAccept || r.Refusal != policy.CodePeer || r.Host != "" {
			t.Errorf("record %+v, want accept/peer", r)
		}
	})
}

func TestTunnel(t *testing.T) {
	echo := newEcho(t)
	e := start(t, open(echo.addr()))
	e.res.set("echo.test", addrs("127.0.0.1"))
	rp := e.send(t, "CONNECT Echo.Test.:"+echo.port()+" HTTP/1.1\r\nHost: echo.test\r\n\r\n")
	if rp.status != http.StatusOK {
		t.Fatalf("CONNECT: %d", rp.status)
	}
	c, br := rp.conn, rp.br
	if _, err := io.WriteString(c, "hello"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "hello" {
		t.Fatalf("echo: %q, %v", got, err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	if rest, err := io.ReadAll(br); err != nil || len(rest) != 0 {
		t.Fatalf("after the half-close: %q, %v", rest, err)
	}
	r := last(t, e.sink.wait(t, 1))
	want := Record{Type: RecordType, Mode: ModeTunnel, Method: http.MethodConnect, Host: "echo.test",
		Port: r.Port, Status: http.StatusOK, Up: 5, Down: 5}
	r.At, r.Duration = time.Time{}, 0
	if r != want || fmt.Sprint(r.Port) != echo.port() {
		t.Errorf("record %+v, want %+v", r, want)
	}
}

func TestForward(t *testing.T) {
	var gotAuth, gotProxyAuth, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotProxyAuth, gotQuery = r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization"), r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "got %d", len(b))
	}))
	t.Cleanup(up.Close)
	e := start(t, open(up.Listener.Addr().String()))
	e.res.set("plain.test", addrs("127.0.0.1"))
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	rp := e.send(t, "POST http://plain.test:"+port+"/p/x?q=query-secret HTTP/1.1\r\n"+
		"Host: plain.test\r\nAuthorization: Bearer sk-live-secret-token\r\n"+
		"Proxy-Authorization: Basic eDp4\r\nX-Other: header-secret\r\nContent-Length: 4\r\n\r\nbody")
	if rp.status != http.StatusCreated || rp.body != "got 4" {
		t.Fatalf("POST: %d %q", rp.status, rp.body)
	}
	if gotAuth != "Bearer sk-live-secret-token" || gotProxyAuth != "" || gotQuery != "q=query-secret" {
		t.Errorf("upstream got Authorization %q, Proxy-Authorization %q, query %q", gotAuth, gotProxyAuth, gotQuery)
	}
	r := last(t, e.sink.wait(t, 1))
	if r.Mode != ModeForward || r.Host != "plain.test" || r.Path != "/p/x" || r.Status != http.StatusCreated ||
		r.Up != 4 || r.Down != 5 || r.Refusal != "" {
		t.Errorf("record %+v", r)
	}
}

// TestForwardCutResponse: an upstream that promises 100 bytes, sends 7 and closes makes the
// ReverseProxy abort the handler (http.ErrAbortHandler); the request is still exactly one
// forward record, with its byte counts.
func TestForwardCutResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n1234567")
	}()
	e := start(t, open(ln.Addr().String()))
	e.res.set("plain.test", addrs("127.0.0.1"))
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "POST http://plain.test:"+port+"/exfil HTTP/1.1\r\nHost: plain.test\r\n"+
		"Content-Length: 6\r\n\r\nabcdef")
	_, _ = io.ReadAll(c) // the aborted handler's connection is closed, whatever it had sent
	e.sink.wait(t, 1)
	e.close(t)
	recs := e.sink.all()
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one", recs)
	}
	if r := recs[0]; r.Mode != ModeForward || r.Host != "plain.test" || r.Path != "/exfil" ||
		r.Status != http.StatusOK || r.Up != 6 || r.Down != 7 || r.Refusal != "" {
		t.Errorf("record %+v, want forward 200 up 6 down 7", r)
	}
}

func TestRefusals(t *testing.T) {
	echo := newEcho(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	e := start(t, open(echo.addr(), closedAddr))
	e.res.set("echo.test", addrs("127.0.0.1"))
	e.res.set("mixed.test", addrs("127.0.0.1", "10.1.2.3"))
	_, closedPort, _ := net.SplitHostPort(closedAddr)
	p := echo.port()
	tests := []struct {
		name, raw string
		code      policy.Code
		mode      Mode
		body      string
	}{
		{"origin form", "GET /x HTTP/1.1\r\nHost: echo.test\r\n\r\n", policy.CodeNotProxy, ModeForward, ""},
		{"absolute https", "GET https://echo.test/ HTTP/1.1\r\nHost: echo.test\r\n\r\n", policy.CodeNotProxy, ModeForward, ""},
		{"no port", "CONNECT echo.test HTTP/1.1\r\nHost: echo.test\r\n\r\n", policy.CodeBadTarget, ModeTunnel, ""},
		{"private literal", "CONNECT 10.0.0.1:" + p + " HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeEgressDenied, ModeTunnel, "(private)"},
		{"mixed answers", "CONNECT mixed.test:" + p + " HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeEgressDenied, ModeTunnel, "(private)"},
		{"no such name", "CONNECT nowhere.test:443 HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeResolve, ModeTunnel, ""},
		{"nothing listening", "CONNECT echo.test:" + closedPort + " HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeUpstream, ModeTunnel, ""},
		{"intercepted, another port", "CONNECT api.anthropic.com:8443 HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodePort, ModeConnect, ""},
		{"auth host", "CONNECT auth.openai.com:443 HTTP/1.1\r\nHost: x\r\n\r\n", policy.CodeAuthHost, ModeConnect, ""},
		{"dispatcher, another task", "POST http://dispatcher.factory.internal/api/v1/tasks/task-2/effects HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", policy.CodeDispatcherRoute, ModeDispatcher, ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := e.send(t, tt.raw)
			want, _ := tt.code.Response(policy.DenyPrivate)
			if rp.status != want.Status || rp.code != tt.code || !strings.Contains(rp.body, tt.body) ||
				!strings.HasPrefix(rp.body, policy.BodyPrefix) {
				t.Fatalf("got %d %s %q, want %d %s", rp.status, rp.code, rp.body, want.Status, tt.code)
			}
			if strings.HasPrefix(tt.raw, http.MethodConnect) {
				_ = rp.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if n, err := rp.br.Read(make([]byte, 1)); n != 0 || err == nil {
					t.Error("a refused CONNECT left the connection open")
				}
			}
			r := e.sink.wait(t, i+1)[i]
			if r.Refusal != tt.code || r.Mode != tt.mode || r.Status != want.Status {
				t.Errorf("record %+v, want %s/%s", r, tt.mode, tt.code)
			}
		})
	}
	if n := echo.count(); n != 0 {
		t.Errorf("a refused request reached the upstream %d times", n)
	}
}

func TestConnectionLimit(t *testing.T) {
	e := start(t, nil, func(g *Gateway) { g.maxConns = 1 })
	dial := func() net.Conn {
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	// The first connection is admitted: it gets an answer and stays open.
	first := dial()
	ask := func(c net.Conn) error {
		_, _ = io.WriteString(c, "GET /x HTTP/1.1\r\nHost: x\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := ask(first); err != nil {
		t.Fatalf("the first connection: %v", err)
	}
	second := dial()
	if n, err := second.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatal("the connection over the limit wasn't closed")
	}
	_ = second.Close()
	_ = first.Close()
	// Closing the first frees its slot (the gateway sees the close asynchronously).
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dial()
		err := ask(c)
		_ = c.Close()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot after the first connection closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEgressDialer(t *testing.T) {
	echo := newEcho(t)
	e := start(t, open(echo.addr()))
	// A rebinding name: public (an open address here) first, private after.
	e.res.set("rebind.test", addrs("127.0.0.1"), addrs("10.0.0.1"))
	target := "rebind.test:" + echo.port()
	c, err := e.g.dialEgress(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if got := c.RemoteAddr().String(); got != echo.addr() {
		t.Errorf("dialled %s, want the checked literal %s", got, echo.addr())
	}
	_ = c.Close()
	if _, err := e.g.dialEgress(context.Background(), "tcp", target); err == nil {
		t.Fatal("the rebound private answer was dialled")
	} else if ee, ok := errors.AsType[*egressError](err); !ok || ee.code != policy.CodeEgressDenied || ee.class != policy.DenyPrivate {
		t.Fatalf("second dial: %v, want egress-denied (private)", err)
	}
	if n := e.res.count("rebind.test"); n != 2 {
		t.Errorf("%d lookups for two dials, want one each", n)
	}
	for deadline := time.Now().Add(5 * time.Second); echo.count() < 1 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond) // the echo counts its accept asynchronously
	}
	if n := echo.count(); n != 1 {
		t.Errorf("the upstream saw %d connections, want 1", n)
	}
	if err := e.g.controlDial("tcp4", "10.0.0.1:80", nil); err == nil {
		t.Error("controlDial passed a private address")
	}
	if err := e.g.controlDial("tcp4", echo.addr(), nil); err != nil {
		t.Errorf("controlDial refused an open address: %v", err)
	}
}

func TestHostAddrsReread(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	reads := 0
	ifs := []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.64.1"), Mask: net.CIDRMask(24, 32)}}
	h := &hostAddrs{clk: clk, read: func() ([]net.Addr, error) { reads++; return ifs, nil }}
	e := policy.Egress{}
	public := netip.MustParseAddrPort("8.8.4.4:443") // only checked, never dialled
	if c := e.CheckAddr(public, h.prefixes()); c != "" {
		t.Fatalf("%v denied before the bridge appeared: %s", public, c)
	}
	ifs = append(ifs, &net.IPNet{IP: net.ParseIP("8.8.4.1"), Mask: net.CIDRMask(24, 32)})
	clk.Advance(10 * time.Second)
	if c := e.CheckAddr(public, h.prefixes()); c != "" || reads != 1 {
		t.Fatalf("re-read within 30 s (%d reads, class %q)", reads, c)
	}
	clk.Advance(21 * time.Second)
	if c := e.CheckAddr(public, h.prefixes()); c != policy.DenyAgentHost || reads != 2 {
		t.Fatalf("after 31 s: %d reads, class %q, want a re-read and agent-host", reads, c)
	}
}

func TestClose(t *testing.T) {
	echo := newEcho(t)
	inflight := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inflight)
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	e := start(t, open(echo.addr(), up.Listener.Addr().String()))
	e.res.set("echo.test", addrs("127.0.0.1"))
	e.res.block["slow.test"] = true
	_, upPort, _ := net.SplitHostPort(up.Listener.Addr().String())

	// A tunnel in mid-copy.
	rp := e.send(t, "CONNECT echo.test:"+echo.port()+" HTTP/1.1\r\nHost: x\r\n\r\n")
	if rp.status != http.StatusOK {
		t.Fatalf("CONNECT: %d", rp.status)
	}
	tun, br := rp.conn, rp.br
	_, _ = io.WriteString(tun, "ab")
	if _, err := io.ReadFull(br, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	// A CONNECT pending in its resolution.
	pending, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pending.Close() }()
	_, _ = io.WriteString(pending, "CONNECT slow.test:443 HTTP/1.1\r\nHost: x\r\n\r\n")
	<-e.res.entered
	// A forwarded request in flight.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "GET http://127.0.0.1:"+upPort+"/slow HTTP/1.1\r\nHost: x\r\n\r\n")
		_, _ = io.ReadAll(c)
	}()
	<-inflight

	began := time.Now()
	e.close(t)
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("Close took %v", d)
	}
	recs := e.sink.all()
	modes := map[Mode]int{}
	for _, r := range recs {
		modes[r.Mode]++
	}
	if len(recs) != 3 || modes[ModeTunnel] != 2 || modes[ModeForward] != 1 {
		t.Errorf("records when Close returned: %+v, want the tunnel, the pending CONNECT and the forward", recs)
	}
	for _, c := range []net.Conn{tun, pending} {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				t.Error("a connection is still open after Close")
			}
		}
	}
	<-done
	if err := e.g.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if got := len(e.sink.all()); got != len(recs) {
		t.Errorf("%d records after Close returned", got-len(recs)) // the sealed sink fails too
	}
}

func TestRecordJSON(t *testing.T) {
	recs := []Record{
		{Type: RecordType, At: time.Date(2026, 10, 22, 10, 0, 0, 123_000_000, time.UTC), Mode: ModeTunnel,
			Method: http.MethodConnect, Host: "registry.npmjs.org", Port: 443, Status: 200,
			Up: 1834, Down: 20511, Duration: 5120 * time.Millisecond},
		{Type: RecordType, At: time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("x", 3600)), Mode: ModeForward,
			Method: http.MethodGet, Host: "10.0.0.1", Port: 80, Path: "/" + strings.Repeat("é", 300), Status: 403,
			Refusal: policy.CodeEgressDenied},
	}
	recs[1].Path = cutPath(recs[1].Path)
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if len(recs[1].Path) > maxPath || !strings.HasSuffix(recs[1].Path, "é") {
		t.Errorf("cutPath: %d bytes, ends %q", len(recs[1].Path), recs[1].Path[len(recs[1].Path)-2:])
	}
	golden := filepath.Join("testdata", "records.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("records:\n%s\nwant:\n%s", b.String(), want)
	}
}
