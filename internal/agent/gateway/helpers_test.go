package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
)

// secrets are values a test sends in carriers, queries and headers: no record and no log line
// may contain one.
var secrets = []string{"sk-live-secret-token", "query-secret", "header-secret"}

// sink is a Sink that keeps every record and fails the test on one after Close returned.
type sink struct {
	mu     sync.Mutex
	recs   []Record
	sealed bool
	t      testing.TB
}

func (s *sink) Egress(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		s.t.Errorf("a record after Close returned: %+v", r)
	}
	s.recs = append(s.recs, r)
}

func (s *sink) all() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.recs...)
}

func (s *sink) seal() {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
}

// wait returns the records once there are at least n, failing after 5 s.
func (s *sink) wait(t testing.TB, n int) []Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rs := s.all(); len(rs) >= n {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d records, have %+v", n, s.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// resolver maps names to answers: the i-th lookup of a name gets answers[i], the last one
// repeating. A name in block waits for its context. Unknown names don't resolve.
type resolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
	block   map[string]bool
	entered chan string
}

func newResolver() *resolver {
	return &resolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}, block: map[string]bool{},
		entered: make(chan string, 16)}
}

func (r *resolver) set(name string, answers ...[]netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[name] = answers
}

func (r *resolver) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func (r *resolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	i := r.calls[host]
	r.calls[host]++
	as, block := r.answers[host], r.block[host]
	r.mu.Unlock()
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	if block {
		r.entered <- host
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(as) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return as[min(i, len(as)-1)], nil
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

var loopback = netip.MustParseAddr("127.0.0.1")

// hostIfaces is the agent host's interfaces the tests' gateways see: a VM bridge.
func hostIfaces() ([]net.Addr, error) {
	return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.64.1"), Mask: net.CIDRMask(24, 32)}}, nil
}

// env is a gateway serving on 127.0.0.1 for the test, with its sink, resolver and log.
type env struct {
	g    *Gateway
	sink *sink
	res  *resolver
	logs *syncBuffer
	addr string
	errc chan error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start serves a gateway whose only client is 127.0.0.1; mut edits the Config first, and gmut
// the gateway before it serves. Cleanup
// closes it, checks Serve returned nil, and checks no record or log line holds a secret.
func start(t *testing.T, mut func(*Config), gmut ...func(*Gateway)) *env {
	t.Helper()
	h := &env{sink: &sink{t: t}, res: newResolver(), logs: &syncBuffer{}, errc: make(chan error, 1)}
	cfg := Config{
		TaskID: "task-1", RunID: "run-1", Clients: []netip.Addr{loopback}, Sink: h.sink, Resolver: h.res, CA: testCA(t),
		Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mut != nil {
		mut(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.ifaces.read = hostIfaces
	for _, m := range gmut {
		m(g)
	}
	h.g = g
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = ln.Addr().String()
	go func() { h.errc <- g.Serve(context.Background(), ln) }()
	t.Cleanup(func() {
		h.close(t)
		h.assertNoSecrets(t)
	})
	return h
}

// close closes the gateway, seals the sink and checks Serve returned nil.
func (h *env) close(t *testing.T) {
	t.Helper()
	if err := h.g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	h.sink.seal()
	select {
	case err := <-h.errc:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
		h.errc <- nil
	case <-time.After(5 * time.Second):
		t.Error("Serve didn't return after Close")
	}
}

func (h *env) assertNoSecrets(t *testing.T) {
	t.Helper()
	logs := h.logs.String()
	for _, r := range h.sink.all() {
		b, _ := json.Marshal(r)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("a record holds %q: %s", s, b)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("the log holds %q", s)
		}
	}
}

// open opens the given addresses (127.0.0.1:port strings) as egress exceptions.
func open(hostports ...string) func(*Config) {
	return func(c *Config) {
		for _, hp := range hostports {
			c.Egress.OpenAddrs = append(c.Egress.OpenAddrs, netip.MustParseAddrPort(hp))
		}
	}
}

// reply is a response the gateway sent; conn stays open for a CONNECT that succeeded.
type reply struct {
	status int
	code   policy.Code
	body   string
	conn   net.Conn
	br     *bufio.Reader
}

// send writes a raw request to the gateway and reads the response.
func (h *env) send(t *testing.T, raw string) reply {
	t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	method := strings.SplitN(raw, " ", 2)[0]
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%q: reading the response: %v", raw, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rp := reply{status: resp.StatusCode, code: policy.Code(resp.Header.Get(policy.RefusalHeader)), conn: c, br: br}
	if method != http.MethodConnect || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		rp.body = string(b)
	}
	return rp
}

// echoServer accepts TCP connections on 127.0.0.1 and echoes each one; it counts accepts.
type echoServer struct {
	ln      net.Listener
	mu      sync.Mutex
	accepts int
}

func newEcho(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echoServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.mu.Lock()
			e.accepts++
			e.mu.Unlock()
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return e
}

func (e *echoServer) addr() string { return e.ln.Addr().String() }

func (e *echoServer) port() string { _, p, _ := net.SplitHostPort(e.addr()); return p }

func (e *echoServer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accepts
}

// last is the last record, failing when there is none.
func last(t *testing.T, rs []Record) Record {
	t.Helper()
	if len(rs) == 0 {
		t.Fatal("no record")
	}
	return rs[len(rs)-1]
}
