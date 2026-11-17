package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/internal/gatewaycases"
)

// modelKey is the model endpoint's key. It holds secrets[0], so assertNoSecrets checks that no
// record or log line has it.
const modelKey = "model-sk-live-secret-token"

// newModelServer is a model server (over TLS when tlsOn) that records each request, then runs
// h, or answers "ok".
func newModelServer(t *testing.T, tlsOn bool, h http.HandlerFunc) (*httptest.Server, *seen) {
	t.Helper()
	got := &seen{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got.add(r.Clone(context.Background()))
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	srv := httptest.NewUnstartedServer(handler)
	if tlsOn {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, got
}

// withModel gives the gateway a model endpoint at rawURL, with modelKey.
func withModel(rawURL string) func(*Config) {
	return func(c *Config) {
		u, _ := url.Parse(rawURL)
		c.ModelEndpoint = &ModelEndpoint{URL: u, APIKey: modelKey}
	}
}

// checkModelRequest fails unless the model server got want as its request URI and exactly the
// key as Bearer, with no other carrier, no Cookie and no placeholder.
func checkModelRequest(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.URL.RequestURI(); got != want {
		t.Errorf("the model server got %s, want %s", got, want)
	}
	if a := r.Header.Values("Authorization"); len(a) != 1 || a[0] != "Bearer "+modelKey {
		t.Errorf("the model server got Authorization %q", a)
	}
	for k, vs := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "X-Api-Key", "Proxy-Authorization", "Cookie":
			t.Errorf("the model server got %s", k)
		}
		for _, v := range vs {
			if strings.Contains(v, harness.Placeholder) || strings.Contains(v, "header-secret") {
				t.Errorf("the model server got %s with the VM's value", k)
			}
		}
	}
}

// TestModelEndpointCases runs gatewaycases' model endpoint rows: the status, the refusal code,
// whether the model server was reached and what it got.
func TestModelEndpointCases(t *testing.T) {
	srv, got := newModelServer(t, false, nil)
	with := start(t, withModel(srv.URL))
	without := start(t, nil)
	for _, c := range gatewaycases.ModelEndpoint() {
		t.Run(c.Name, func(t *testing.T) {
			e := with
			if c.NoEndpoint {
				e = without
			}
			before := len(got.all())
			rp := e.send(t, c.Raw)
			if rp.status != c.Status || string(rp.code) != c.Refusal {
				t.Fatalf("got %d %q, want %d %q", rp.status, rp.code, c.Status, c.Refusal)
			}
			if strings.Contains(rp.body, modelKey) {
				t.Error("the VM got the key")
			}
			reqs := got.all()[before:]
			switch {
			case c.Upstream == "" && len(reqs) != 0:
				t.Errorf("a refused request reached the model server")
			case c.Upstream != "" && len(reqs) != 1:
				t.Errorf("the model server got %d requests, want 1", len(reqs))
			case c.Upstream != "":
				checkModelRequest(t, reqs[0], c.Upstream)
			}
		})
	}

	t.Run("the egress dialer refuses the server's address", func(t *testing.T) {
		before := len(got.all())
		rp := with.send(t, "GET "+srv.URL+"/v1/models HTTP/1.1\r\nHost: x\r\n\r\n")
		if rp.code != policy.CodeEgressDenied || len(got.all()) != before {
			t.Errorf("got %d %s; the model server is reachable outside its route", rp.status, rp.code)
		}
	})
	t.Run("the transport dials only the model server", func(t *testing.T) {
		if _, err := with.g.model.DialContext(context.Background(), "tcp", "127.0.0.1:9"); !errors.Is(err, errModelAddr) {
			t.Errorf("dialled another address: %v", err)
		}
	})
}

// TestModelEndpointRoute: chat completions reach a 127.0.0.1 model server (which the egress
// dialer refuses), over http and over https through RootCAs or, without them, UpstreamRoots,
// under deny-by-default with an empty allow list, with the URL's path joined in front of the
// request's, every carrier and Cookie replaced by the key, and a record that names the route,
// never the server.
func TestModelEndpointRoute(t *testing.T) {
	for _, tt := range []struct {
		name            string
		tlsOn, ownRoots bool
	}{{"http", false, false}, {"https through RootCAs", true, true}, {"https through UpstreamRoots", true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newModelServer(t, tt.tlsOn, nil)
			e := start(t, func(c *Config) {
				withModel(srv.URL + "/prefix/")(c)
				c.Egress.Mode = policy.ModeDenyByDefault // the route isn't subject to it
				if tt.tlsOn {
					roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
					if tt.ownRoots {
						c.ModelEndpoint.RootCAs = roots
					} else {
						c.UpstreamRoots = roots
					}
				}
			})
			rp := e.send(t, "POST http://model.factory.internal/v1/chat/completions?a=1 HTTP/1.1\r\n"+
				"Host: model.factory.internal\r\nAuthorization: Bearer header-secret\r\nX-Api-Key: header-secret\r\n"+
				"Cookie: s=header-secret\r\nContent-Length: 2\r\n\r\n{}")
			if rp.status != http.StatusOK || rp.body != "ok" {
				t.Fatalf("got %d %s %q", rp.status, rp.code, rp.body)
			}
			r := got.all()[0]
			checkModelRequest(t, r, "/prefix/v1/chat/completions?a=1")
			if r.Host != srv.Listener.Addr().String() {
				t.Errorf("the model server got Host %q", r.Host)
			}
			rec := last(t, e.sink.wait(t, 1))
			want := Record{Type: RecordType, At: rec.At, Mode: ModeModel, Method: http.MethodPost, Host: policy.ModelEndpointHost,
				Port: 80, Path: "/v1/chat/completions", Status: http.StatusOK, Injected: InjectedModelEndpoint,
				Client: ClientOther, Up: 2, Down: 2, Duration: rec.Duration}
			if rec != want {
				t.Errorf("record %+v, want %+v", rec, want)
			}
		})
	}
}

// TestModelEndpointStreams: an event stream reaches the VM as it comes: the client reads the
// first event before the server sends the last.
func TestModelEndpointStreams(t *testing.T) {
	release := make(chan struct{})
	srv, _ := newModelServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	e := start(t, withModel(srv.URL))
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "POST http://model.factory.internal/v1/chat/completions HTTP/1.1\r\n"+
		"Host: model.factory.internal\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := bufio.NewReader(resp.Body)
	var first string
	for !strings.HasSuffix(first, "\n\n") {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the first event before the last was sent: %v (got %q)", err, first+line)
		}
		first += line
	}
	if first != "data: first\n\n" {
		t.Fatalf("the first event is %q", first)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(body)
	if err != nil || string(rest) != "data: [DONE]\n\n" {
		t.Errorf("the rest of the stream is %q (%v)", rest, err)
	}
}
