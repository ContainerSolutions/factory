package github_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// memorySink stores deliveries by ID, like the provider controller: a second one with the
// same ID is ErrDuplicateDelivery.
type memorySink struct {
	mu   sync.Mutex
	got  []provider.RawEvent
	ids  map[string]bool
	fail error
}

func newSink(ids ...string) *memorySink {
	s := &memorySink{ids: map[string]bool{}}
	for _, id := range ids {
		s.ids[id] = true
	}
	return s
}

func (s *memorySink) Accept(_ context.Context, ev provider.RawEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.ids[ev.ID] {
		return fmt.Errorf("delivery %s: %w", ev.ID, provider.ErrDuplicateDelivery)
	}
	s.ids[ev.ID] = true
	s.got = append(s.got, ev)
	return nil
}

func (s *memorySink) accepted() []provider.RawEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func TestHandler(t *testing.T) {
	body, err := os.ReadFile("testdata/webhooks/issues.opened.json")
	if err != nil {
		t.Fatal(err)
	}
	good := sign(hookSecret, body)
	const guid = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
	tests := []struct {
		name     string
		method   string
		body     []byte
		sig      string
		delivery string
		event    string
		sink     func() provider.IngressSink
		want     int
		stored   bool
	}{
		{name: "accepted", sig: good, want: http.StatusAccepted, stored: true},
		{name: "duplicate", sig: good, sink: func() provider.IngressSink { return newSink("gh-" + guid) }, want: http.StatusOK},
		{name: "sink fails", sig: good, sink: func() provider.IngressSink {
			s := newSink()
			s.fail = errors.New("store full")
			return s
		}, want: http.StatusServiceUnavailable},
		{name: "no sink", sig: good, sink: func() provider.IngressSink { return nil }, want: http.StatusServiceUnavailable},
		{name: "missing signature", want: http.StatusUnauthorized},
		{name: "wrong secret", sig: sign("not-the-secret", body), want: http.StatusUnauthorized},
		{name: "tampered body", sig: good, body: append(slices.Clone(body), ' '), want: http.StatusUnauthorized},
		{name: "sha1 signature", sig: "sha1=" + strings.Repeat("0", 40), want: http.StatusUnauthorized},
		{name: "not hex", sig: "sha256=" + strings.Repeat("z", 64), want: http.StatusUnauthorized},
		{name: "short", sig: good[:20], want: http.StatusUnauthorized},
		{name: "GET", method: http.MethodGet, sig: good, want: http.StatusMethodNotAllowed},
		{name: "no delivery id", sig: good, delivery: "-", want: http.StatusBadRequest},
		{name: "bad delivery id", sig: good, delivery: "../etc", want: http.StatusBadRequest},
		{name: "no event", sig: good, event: "-", want: http.StatusBadRequest},
		{name: "too large", body: bytes.Repeat([]byte("x"), github.MaxWebhookBytes+1), want: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			e := newEnv(t)
			e.adapter = github.NewAdapter(github.Options{
				HTTPClient: e.srv.Client(), Clock: e.clk,
				Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			c := e.connect(t)
			b := body
			if tt.body != nil {
				b = tt.body
			}
			method := http.MethodPost
			if tt.method != "" {
				method = tt.method
			}
			req := httptest.NewRequest(method, "/hooks/github-public", bytes.NewReader(b))
			if tt.sig != "" {
				req.Header.Set("X-Hub-Signature-256", tt.sig)
			}
			req.Header.Set("X-GitHub-Delivery", guid)
			if tt.delivery == "-" {
				req.Header.Del("X-GitHub-Delivery")
			} else if tt.delivery != "" {
				req.Header.Set("X-GitHub-Delivery", tt.delivery)
			}
			req.Header.Set("X-GitHub-Event", "issues")
			if tt.event == "-" {
				req.Header.Del("X-GitHub-Event")
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "should-not-be-stored")
			sink := newSink()
			var s provider.IngressSink = sink
			if tt.sink != nil {
				s = tt.sink()
			}
			rec := httptest.NewRecorder()
			c.Handler(s).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
			got := sink.accepted()
			if !tt.stored {
				if len(got) != 0 {
					t.Errorf("stored %d deliveries, want none", len(got))
				}
			} else {
				if len(got) != 1 {
					t.Fatalf("stored %d deliveries, want 1", len(got))
				}
				ev := got[0]
				if ev.ID != "gh-"+guid || ev.Provider != prov || ev.Type != "issues" || !bytes.Equal(ev.Body, body) ||
					!ev.ReceivedAt.Equal(t0) || ev.Headers.Get("X-Hub-Signature-256") != good || ev.Headers.Get("Authorization") != "" {
					t.Errorf("stored %+v", ev)
				}
			}
			if strings.Contains(logs.String(), "Ignore previous instructions") || strings.Contains(logs.String(), hookSecret) {
				t.Errorf("logs leak the payload or the secret:\n%s", logs.String())
			}
		})
	}
}

// TestHandlerAcceptsBeforeAcking checks that the handler answers only after Accept returned.
func TestHandlerAcceptsBeforeAcking(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	release := make(chan struct{})
	entered := make(chan struct{})
	sink := provider.IngressSinkFunc(func(context.Context, provider.RawEvent) error {
		close(entered)
		<-release
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-public", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(hookSecret, body))
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		c.Handler(sink).ServeHTTP(rec, req)
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("the handler answered before Accept returned")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

func TestRecover(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	since := t0.Add(-time.Hour)
	issue, err := os.ReadFile("testdata/webhooks/issues.opened.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, issue); err != nil {
		t.Fatal(err)
	}
	hdr := func(event, guid string) map[string]string {
		return map[string]string{"X-GitHub-Event": event, "X-GitHub-Delivery": guid, "Content-Type": "application/json", "X-Hub-Signature-256": "sha256=00"}
	}
	ping := json.RawMessage(`{"zen":"Design for failure."}`)
	for _, d := range []githubtest.Delivery{
		{ID: 1, GUID: "g-old", DeliveredAt: since.Add(-time.Minute), Event: "ping", StatusCode: 202, Headers: hdr("ping", "g-old"), Payload: ping},
		{ID: 2, GUID: "g-2", DeliveredAt: since.Add(time.Minute), Event: "ping", StatusCode: 202, Headers: hdr("ping", "g-2"), Payload: ping},
		{ID: 3, GUID: "g-3", DeliveredAt: since.Add(2 * time.Minute), Event: "issues", Action: "opened", StatusCode: 503, Headers: hdr("issues", "g-3"), Payload: compact.Bytes()},
		{ID: 4, GUID: "g-3", DeliveredAt: since.Add(3 * time.Minute), Event: "issues", Action: "opened", Redelivery: true, StatusCode: 202, Headers: hdr("issues", "g-3"), Payload: compact.Bytes()},
		{ID: 5, GUID: "g-stored", DeliveredAt: since.Add(4 * time.Minute), Event: "ping", StatusCode: 202, Headers: hdr("ping", "g-stored"), Payload: ping},
		{ID: 6, GUID: "g-empty", DeliveredAt: since.Add(5 * time.Minute), Event: "ping", StatusCode: 202, Headers: hdr("ping", "g-empty")},
	} {
		e.srv.AddDelivery(authorApp, d)
	}
	e.srv.SetPageSize(2) // three pages of deliveries
	sink := newSink("gh-g-stored")
	if err := c.Recover(context.Background(), since, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.accepted()
	var ids []string
	for _, ev := range got {
		ids = append(ids, ev.ID)
	}
	if want := []string{"gh-g-2", "gh-g-3"}; !slices.Equal(ids, want) {
		t.Fatalf("recovered %v, want %v (oldest first, once per GUID, stored and empty ones skipped)", ids, want)
	}
	ev := got[1]
	if ev.Provider != prov || ev.Type != "issues" || !bytes.Equal(ev.Body, compact.Bytes()) ||
		!ev.ReceivedAt.Equal(since.Add(2*time.Minute)) || ev.Headers.Get("X-GitHub-Delivery") != "g-3" {
		t.Errorf("recovered delivery = %+v", ev)
	}
	evs, err := c.Normalise(context.Background(), ev)
	if err != nil || len(evs) != 1 || evs[0].ID != "gh-g-3/0" {
		t.Errorf("Normalise(recovered) = %v, %v", evs, err)
	}
	// The reviewer App has no webhook: 404, nothing to recover.
	if n := e.srv.Count(githubtest.RouteDeliveries); n != 3+1 {
		t.Errorf("%d delivery listings, want 3 pages for the author and 1 for the reviewer", n)
	}

	// Recovering again sends nothing new.
	if err := c.Recover(context.Background(), since, sink); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.accepted()); n != 2 {
		t.Errorf("second Recover accepted %d in all, want 2", n)
	}

	// A sink error stops Recover; a listing error is returned.
	failing := newSink()
	failing.fail = errors.New("store down")
	if err := c.Recover(context.Background(), since, failing); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Errorf("Recover with a failing sink = %v", err)
	}
	e.srv.FailNext(githubtest.RouteDeliveries, githubtest.Failure{Status: 502})
	if err := c.Recover(context.Background(), since, newSink()); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("Recover with GitHub down = %v, want retryable", err)
	}
	if err := c.Recover(context.Background(), since, nil); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("Recover(nil sink) = %v", err)
	}
}
