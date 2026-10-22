package runbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func fixedClock() func() time.Time { return func() time.Time { return t0 } }

// stream is a run's events, as vmrun sends them.
func stream() []Event {
	cost := 0.25
	return []Event{
		{Type: EventStart, Start: &Start{Version: Version, RunID: "run_1-1", TaskID: "tsk_1"}},
		{Type: EventStep, Step: StepOf("prompt", 1500*time.Millisecond, nil)},
		{Type: EventProgress, At: t0.Add(time.Second), Progress: &v1alpha1.ProgressEvent{At: t0.Add(time.Second), Kind: "tool", Message: "go test ./..."}},
		{Type: EventUsage, Usage: UsageOf(runner.UsageDelta{InputTokens: 1200, OutputTokens: 300, CostUSD: &cost})},
		{Type: EventStep, Step: StepOf("harness", 2*time.Minute, errors.New("exit status 1"))},
		OutcomeEvent(Outcome{Status: runner.StatusFailed, Reason: runner.ReasonHarnessError, Transcript: TranscriptFile, Result: &v1alpha1.Result{Summary: strings.Repeat("a", 2*MaxEventBytes)}}),
		{Type: EventStep, Step: StepOf("environment.down", 3*time.Second, nil)},
	}
}

func TestEventsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, fixedClock())
	for _, e := range stream() {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("Encode(%s): %v", e.Type, err)
		}
	}
	checkGolden(t, "events.golden", buf.Bytes())

	dec := NewDecoder(&buf)
	for i, want := range stream() {
		want.V = EventVersion
		if want.At.IsZero() {
			want.At = t0
		}
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		got.At = got.At.UTC()
		if got.Progress != nil {
			got.Progress.At = got.Progress.At.UTC()
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("event %d = %+v, want %+v", i, got, want)
		}
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the stream: %v, want io.EOF", err)
	}
	if u := RunnerUsage(*stream()[3].Usage); u.InputTokens != 1200 || u.OutputTokens != 300 || *u.CostUSD != 0.25 {
		t.Errorf("RunnerUsage = %+v", u)
	}
}

// progressOf is a progress event whose line is n bytes, its newline included.
func progressOf(t *testing.T, n int) Event {
	t.Helper()
	e := Event{V: EventVersion, Type: EventProgress, At: t0, Progress: &v1alpha1.ProgressEvent{At: t0, Kind: "tool", Message: "x"}}
	base, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Progress.Message = strings.Repeat("a", n-len(base))
	return e
}

func TestEncoderLineCap(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, fixedClock())
	if err := enc.Encode(progressOf(t, MaxEventBytes)); err != nil {
		t.Fatalf("a line of exactly MaxEventBytes: %v", err)
	}
	if buf.Len() != MaxEventBytes {
		t.Fatalf("the line is %d bytes, want %d", buf.Len(), MaxEventBytes)
	}
	if _, err := NewDecoder(bytes.NewReader(buf.Bytes())).Next(); err != nil {
		t.Errorf("decoding a line of exactly MaxEventBytes: %v", err)
	}
	buf.Reset()
	wantInvalid(t, enc.Encode(progressOf(t, MaxEventBytes+1)), "event")
	if buf.Len() != 0 {
		t.Errorf("an oversize event wrote %d bytes", buf.Len())
	}
	wantInvalid(t, enc.Encode(Event{Type: EventStep}), "event")
	wantInvalid(t, enc.Encode(Event{Type: EventOutcome, Outcome: &Outcome{Status: runner.StatusDone, Result: &v1alpha1.Result{}}}), "event.outcome.result")
	wantInvalid(t, enc.Encode(Event{Type: EventOutcome, Outcome: &Outcome{Status: "great"}}), "outcome.status")
}

func TestEncoderConcurrent(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, fixedClock())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if err := enc.Encode(progressOf(t, 4096)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	dec := NewDecoder(&buf)
	n := 0
	for {
		_, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("event %d: %v", n, err)
		}
		n++
	}
	if n != 400 {
		t.Errorf("decoded %d events, want 400", n)
	}
}

func TestDecoder(t *testing.T) {
	line := func(e Event) string {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	start := Event{V: EventVersion, Type: EventStart, At: t0, Start: &Start{Version: Version, RunID: "run_1-1", TaskID: "tsk_1"}}
	ok := line(start)
	type result struct {
		typ   string // the event's type, or ""
		field string // the *ValidationError's field, or ""
	}
	tests := []struct {
		name  string
		input string
		want  []result
	}{
		{"an oversize line, then an event", strings.Repeat("a", 3*MaxEventBytes) + "\n" + ok, []result{{field: "event"}, {typ: EventStart}}},
		{"an oversize last line", ok + strings.Repeat("a", MaxEventBytes+1), []result{{typ: EventStart}, {field: "event"}}},
		{"an unknown version", strings.Replace(ok, `"v":1`, `"v":2`, 1) + ok, []result{{field: "event.v"}, {typ: EventStart}}},
		{"no version", strings.Replace(ok, `"v":1,`, ``, 1), []result{{field: "event.v"}}},
		{"an unknown type is skipped", `{"v":1,"type":"telemetry","at":"2026-10-22T12:00:00Z","telemetry":{}}` + "\n" + ok, []result{{typ: EventStart}}},
		{"blank lines", "\n  \n" + ok + "\n", []result{{typ: EventStart}}},
		{"no trailing newline", strings.TrimSuffix(ok, "\n"), []result{{typ: EventStart}}},
		{"not JSON", "{\n" + ok, []result{{field: "event"}, {typ: EventStart}}},
		{"no payload", `{"v":1,"type":"step","at":"2026-10-22T12:00:00Z"}` + "\n", []result{{field: "event"}}},
		{"another type's payload", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{},"step":{"name":"x","state":"ok","ms":1}}` + "\n", []result{{field: "event"}}},
		{"a bad step state", `{"v":1,"type":"step","at":"2026-10-22T12:00:00Z","step":{"name":"x","state":"meh","ms":1}}` + "\n", []result{{field: "event.step.state"}}},
		{"an outcome with a Result", `{"v":1,"type":"outcome","at":"2026-10-22T12:00:00Z","outcome":{"status":"done","result":{"task":"tsk_1"}}}` + "\n", []result{{field: "event.outcome.result"}}},
		{"a token for a time", `{"v":1,"type":"step","at":"` + ghsToken + `","step":{"name":"x","state":"ok","ms":1}}` + "\n", []result{{field: "event"}}},
		{"a token for a state", `{"v":1,"type":"step","at":"2026-10-22T12:00:00Z","step":{"name":"x","state":"` + ghsToken + `","ms":1}}` + "\n", []result{{field: "event.step.state"}}},
		{"a bad outcome", `{"v":1,"type":"outcome","at":"2026-10-22T12:00:00Z","outcome":{"status":"done","transcript":"/etc/passwd"}}` + "\n", []result{{field: "outcome.transcript"}}},
		{"a throttle time on a failure", `{"v":1,"type":"outcome","at":"2026-10-22T12:00:00Z","outcome":{"status":"failed","throttledUntil":"9999-12-31T23:59:59Z"}}` + "\n", []result{{field: "outcome.throttledUntil"}}},
		{"negative input tokens", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{"inputTokens":-9000000000000000000}}` + "\n" + ok, []result{{field: "event.usage"}, {typ: EventStart}}},
		{"negative output tokens", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{"inputTokens":1,"outputTokens":-1}}` + "\n", []result{{field: "event.usage"}}},
		{"a negative cost", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{"costUSD":-5}}` + "\n", []result{{field: "event.usage"}}},
		{"a cost over float32", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{"costUSD":1e999}}` + "\n", []result{{field: "event"}}},
		{"zero usage", `{"v":1,"type":"usage","at":"2026-10-22T12:00:00Z","usage":{"inputTokens":0,"outputTokens":0,"costUSD":0}}` + "\n", []result{{typ: EventUsage}}},
		{"a 200-digit number", `{"v":1,"type":"step","at":"2026-10-22T12:00:00Z","step":{"name":"x","state":"ok","ms":` + strings.Repeat("9", 200) + `}}` + "\n", []result{{field: "event"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := NewDecoder(strings.NewReader(tt.input))
			for i, w := range tt.want {
				e, err := dec.Next()
				if w.field != "" {
					wantInvalid(t, err, w.field)
					if strings.Contains(err.Error(), "9999") {
						t.Errorf("the error quotes the number: %.80s…", err)
					}
					continue
				}
				if err != nil || e.Type != w.typ {
					t.Fatalf("event %d = %q, %v; want %q", i, e.Type, err, w.typ)
				}
			}
			if _, err := dec.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("at the end: %v, want io.EOF", err)
			}
		})
	}
}

func TestStepOfTruncates(t *testing.T) {
	long := "x" + strings.Repeat("é", 50<<10) // 100 KiB, a rune boundary off MaxStepError
	s := StepOf("harness", time.Second, errors.New(long))
	if s.State != StepError || !strings.HasSuffix(s.Error, truncatedMark) {
		t.Fatalf("Step = %.40q…, want a truncated error", s.Error)
	}
	body := strings.TrimSuffix(s.Error, truncatedMark)
	if len(body) > MaxStepError || len(body) < MaxStepError-utf8.UTFMax || !utf8.ValidString(s.Error) || !strings.HasPrefix(long, body) {
		t.Errorf("the error is %d bytes before its mark (valid UTF-8 %v), want at most %d", len(body), utf8.ValidString(s.Error), MaxStepError)
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf, fixedClock()).Encode(Event{Type: EventStep, Step: s}); err != nil {
		t.Errorf("Encode: %v", err)
	}
	exact := strings.Repeat("a", MaxStepError)
	if got := StepOf("x", 0, errors.New(exact)).Error; got != exact {
		t.Errorf("an error of exactly MaxStepError was cut to %d bytes", len(got))
	}
}
