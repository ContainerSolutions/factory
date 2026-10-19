package fake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

type sink struct {
	mu       sync.Mutex
	progress []harness.ProgressEvent
	usage    []harness.UsageDelta
	limited  []time.Time
}

func (s *sink) Progress(e harness.ProgressEvent) {
	s.mu.Lock()
	s.progress = append(s.progress, e)
	s.mu.Unlock()
}
func (s *sink) Usage(u harness.UsageDelta) { s.mu.Lock(); s.usage = append(s.usage, u); s.mu.Unlock() }
func (s *sink) RateLimited(t time.Time)    { s.mu.Lock(); s.limited = append(s.limited, t); s.mu.Unlock() }

func TestAdapter(t *testing.T) {
	dir := t.TempDir()
	ws := harness.Workspace{Result: filepath.Join(dir, "result.json")}
	a := &Adapter{Script: Script{
		Turn: Turn{
			Events:     []harness.ProgressEvent{{Kind: harness.KindTurn}},
			Usage:      []harness.UsageDelta{{InputTokens: 1}},
			Result:     []byte(`{"status":"done"}`),
			Output:     []byte(`{"x":1}`),
			Transcript: []string{`{"a":1}`, `{"b":2}`},
		},
		Repairs: []Turn{{Err: errors.New("repair failed")}},
	}}
	var _ harness.Adapter = a
	if a.Name() != Name || a.Capabilities().Harness != Name {
		t.Error("name")
	}
	off, err := a.Probe(context.Background(), config.Harness{Models: []string{"m"}, Default: "m"})
	if err != nil || off.DefaultModel != "m" || len(off.Models) != 1 {
		t.Errorf("probe %+v %v", off, err)
	}
	if err := a.Prepare(context.Background(), ws, v1alpha1.Task{}); err != nil || len(a.Prepared()) != 1 {
		t.Fatal("prepare")
	}
	s := &sink{}
	raw, err := a.Run(context.Background(), ws, v1alpha1.Task{}, "prompt", s)
	if err != nil || string(raw.Result) != `{"x":1}` || string(raw.Transcript) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Fatalf("run %q %q %v", raw.Result, raw.Transcript, err)
	}
	if b, _ := os.ReadFile(ws.Result); string(b) != `{"status":"done"}` || len(s.progress) != 1 || len(s.usage) != 1 {
		t.Errorf("result %q, sink %+v", b, s)
	}
	if _, err := a.Repair(context.Background(), ws, []string{"p"}, s); err == nil {
		t.Error("the scripted repair error was lost")
	}
	if _, err := a.Repair(context.Background(), ws, []string{"q"}, s); err != nil {
		t.Errorf("a repair past the script: %v", err)
	}
	if p := a.Problems(); len(p) != 2 || p[1][0] != "q" || a.Prompts()[0] != "prompt" {
		t.Errorf("recorded %v %v", p, a.Prompts())
	}
}

func TestAdapterRateLimitAndBlock(t *testing.T) {
	until := time.Unix(100, 0)
	for _, tt := range []Turn{
		{Events: []harness.ProgressEvent{{}, {}}, RateLimitAfter: 1, RateLimitUntil: until},
		{RateLimitAfter: 1, RateLimitUntil: until},
		{Block: true},
	} {
		a := &Adapter{Script: Script{Turn: tt}}
		s := &sink{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := a.Run(ctx, harness.Workspace{}, v1alpha1.Task{}, "", s)
			done <- err
		}()
		if tt.RateLimitAfter > 0 {
			for {
				s.mu.Lock()
				n := len(s.limited)
				s.mu.Unlock()
				if n == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("%+v: %v", tt, err)
		}
		if tt.RateLimitAfter > 0 && (!s.limited[0].Equal(until) || len(s.progress) > tt.RateLimitAfter) {
			t.Errorf("rate limit %+v", s)
		}
	}
}
