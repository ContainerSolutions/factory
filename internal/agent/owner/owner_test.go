package owner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/internal/fakegh"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/clock"
)

// dispatcher serves GET /api/v1/auth/device-config.
func dispatcher(t *testing.T) *apiclient.ClientWithResponses {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/device-config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiclient.DeviceConfig{ClientId: "client-1", Scopes: []string{"read:user"}})
	}))
	t.Cleanup(ts.Close)
	c, err := apiclient.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// drive advances the fake clock whenever the flow waits, until done.
func drive(clk *clock.Fake, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		if clk.Timers() > 0 {
			clk.Advance(time.Second)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
}

func run(t *testing.T, gh *fakegh.Server, clk *clock.Fake) (string, []string, error) {
	t.Helper()
	var prompts []string
	d := &DeviceFlow{
		API: dispatcher(t), GitHubURL: gh.URL, Clock: clk,
		Prompt: func(uri, code string, _ time.Duration) { prompts = append(prompts, uri+" "+code) },
	}
	done := make(chan struct{})
	var tok string
	var err error
	go func() { defer close(done); tok, err = d.OwnerToken(context.Background()) }()
	drive(clk, done)
	return tok, prompts, err
}

func TestDeviceFlow(t *testing.T) {
	gh := fakegh.New(t, "gus", 2)
	gh.SetSlowDown()
	clk := clock.NewFake(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	start := clk.Now()
	tok, prompts, err := run(t, gh, clk)
	if err != nil || tok != "tok-gus" {
		t.Fatalf("OwnerToken = %q, %v", tok, err)
	}
	if id, scope := gh.Client(); id != "client-1" || scope != "read:user" {
		t.Errorf("device code request: client %q scope %q", id, scope)
	}
	if len(prompts) != 1 || prompts[0] != gh.URL+"/login/device "+fakegh.UserCode {
		t.Errorf("prompts %v", prompts)
	}
	// 4 polls: slow_down (interval 5s → 10s), pending, pending, token.
	if gh.Polls() != 4 {
		t.Errorf("polls = %d, want 4", gh.Polls())
	}
	if waited := clk.Now().Sub(start); waited < 35*time.Second {
		t.Errorf("waited %s, want ≥ 35s (5s, then 10s after slow_down)", waited)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	gh := fakegh.New(t, "gus", 0)
	gh.SetDeny()
	if _, _, err := run(t, gh, clock.NewFake(time.Unix(0, 0))); !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
}

func TestDeviceFlowExpired(t *testing.T) {
	gh := fakegh.New(t, "gus", 1<<30)
	if _, _, err := run(t, gh, clock.NewFake(time.Unix(0, 0))); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestDeviceFlowCancelled(t *testing.T) {
	gh := fakegh.New(t, "gus", 1<<30)
	clk := clock.NewFake(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	d := &DeviceFlow{API: dispatcher(t), GitHubURL: gh.URL, Clock: clk}
	errc := make(chan error, 1)
	go func() { _, err := d.OwnerToken(ctx); errc <- err }()
	clk.BlockUntil(1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDeviceFlowNoDispatcherConfig(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	c, _ := apiclient.NewClientWithResponses(ts.URL)
	d := &DeviceFlow{API: c, GitHubURL: ts.URL, Clock: clock.NewFake(time.Unix(0, 0))}
	if _, err := d.OwnerToken(context.Background()); err == nil {
		t.Fatal("no error without a device config")
	}
}
