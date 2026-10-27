package runbundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

func TestOutcomeRoundTrip(t *testing.T) {
	until := t0.Add(time.Hour)
	tests := []Outcome{
		{Status: runner.StatusDone, Result: &v1alpha1.Result{Task: "tsk_1", Status: v1alpha1.ResultDone, Summary: "did it"}, Transcript: TranscriptFile},
		{Status: runner.StatusFailed, Reason: runner.ReasonCancelled},
		{Status: runner.StatusThrottled, ThrottledUntil: &until, Transcript: TranscriptFile},
	}
	for _, o := range tests {
		t.Run(o.Status, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), OutDir)
			if err := WriteOutcome(out, o); err != nil {
				t.Fatalf("WriteOutcome: %v", err)
			}
			got, err := ReadOutcome(out, t0)
			if err != nil {
				t.Fatalf("ReadOutcome: %v", err)
			}
			if got.ThrottledUntil != nil {
				u := got.ThrottledUntil.UTC()
				got.ThrottledUntil = &u
			}
			if !reflect.DeepEqual(got, o) {
				t.Errorf("got %+v, want %+v", got, o)
			}
			entries, _ := os.ReadDir(out)
			if len(entries) != 1 {
				t.Errorf("out/ has %d entries, want only %s", len(entries), OutcomeFile)
			}
			if r := OutcomeOf(o.Runner(), o.Transcript); !reflect.DeepEqual(r, o) {
				t.Errorf("OutcomeOf(Runner()) = %+v", r)
			}
		})
	}
}

func TestOutcomeRefuses(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		field string
	}{
		{"an unknown status", `{"status":"great"}`, "outcome.status"},
		{"no status", `{}`, "outcome.status"},
		{"an unknown reason", `{"status":"failed","reason":"bored"}`, "outcome.reason"},
		{"a transcript elsewhere", `{"status":"done","transcript":"../../etc/passwd"}`, "outcome.transcript"},
		{"not JSON", `{`, OutcomeFile},
		{"a token for a status", `{"status":"` + ghsToken + `"}`, "outcome.status"},
		{"a token for a time", `{"status":"throttled","throttledUntil":"` + ghsToken + `"}`, OutcomeFile},
		{"a token for a reason", `{"status":"failed","reason":"` + ghsToken + `"}`, "outcome.reason"},
		{"a throttle time on a failure", `{"status":"failed","throttledUntil":"2026-10-22T13:00:00Z"}`, "outcome.throttledUntil"},
		{"a throttle time on done", `{"status":"done","throttledUntil":"2026-10-22T13:00:00Z"}`, "outcome.throttledUntil"},
		{"an oversize outcome", `{"status":"done","result":{"summary":"` + strings.Repeat("a", MaxOutcomeBytes) + `"}}`, OutcomeFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := t.TempDir()
			if err := os.WriteFile(filepath.Join(out, OutcomeFile), []byte(tt.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ReadOutcome(out, t0)
			wantInvalid(t, err, tt.field)
			if o.Status != "" {
				t.Errorf("ReadOutcome returned %+v", o)
			}
		})
	}

	t.Run("a symlink", func(t *testing.T) {
		out := t.TempDir()
		if err := os.Symlink("/etc/hosts", filepath.Join(out, OutcomeFile)); err != nil {
			t.Fatal(err)
		}
		_, err := ReadOutcome(out, t0)
		wantInvalid(t, err, OutcomeFile)
	})

	t.Run("a symlinked out dir", func(t *testing.T) {
		other := t.TempDir() // a sibling run's out/, with a valid outcome
		if err := WriteOutcome(other, Outcome{Status: runner.StatusDone}); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), OutDir)
		if err := os.Symlink(other, out); err != nil {
			t.Fatal(err)
		}
		o, err := ReadOutcome(out, t0)
		wantInvalid(t, err, OutDir)
		if o.Status != "" {
			t.Errorf("ReadOutcome returned %+v", o)
		}
	})

	t.Run("a file for the out dir", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), OutDir)
		if err := os.WriteFile(out, []byte(`{"status":"done"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadOutcome(out, t0)
		wantInvalid(t, err, OutDir)
	})

	t.Run("an outcome with a second link", func(t *testing.T) {
		out := t.TempDir()
		other := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(other, []byte(`{"status":"done"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, filepath.Join(out, OutcomeFile)); err != nil {
			t.Fatal(err)
		}
		_, err := ReadOutcome(out, t0)
		wantInvalid(t, err, OutcomeFile)
	})

	t.Run("WriteOutcome", func(t *testing.T) {
		out := t.TempDir()
		wantInvalid(t, WriteOutcome(out, Outcome{Status: "great"}), "outcome.status")
		until := t0
		wantInvalid(t, WriteOutcome(out, Outcome{Status: runner.StatusNoop, ThrottledUntil: &until}), "outcome.throttledUntil")
		big := Outcome{Status: runner.StatusDone, Result: &v1alpha1.Result{Summary: strings.Repeat("a", MaxOutcomeBytes)}}
		wantInvalid(t, WriteOutcome(out, big), OutcomeFile)
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Errorf("out/ has %d entries", len(entries))
		}
	})
}

// TestOutcomeReasons keeps the outcome's sets runner's: every runner status, and every
// reason on a failure, is valid.
func TestOutcomeReasons(t *testing.T) {
	for _, s := range runner.Statuses() {
		if err := (Outcome{Status: s}).Validate(); err != nil {
			t.Errorf("status %s: %v", s, err)
		}
	}
	for _, r := range runner.Reasons() {
		if err := (Outcome{Status: runner.StatusFailed, Reason: r}).Validate(); err != nil {
			t.Errorf("reason %s: %v", r, err)
		}
	}
}

func TestThrottleClamped(t *testing.T) {
	at := func(d time.Duration) *time.Time { u := t0.Add(d); return &u }
	far := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	tests := []struct {
		name  string
		until *time.Time
		want  *time.Time
	}{
		{"the far future", &far, at(MaxThrottle)},
		{"just over the bound", at(MaxThrottle + time.Nanosecond), at(MaxThrottle)},
		{"the bound", at(MaxThrottle), at(MaxThrottle)},
		{"within the bound", at(time.Hour), at(time.Hour)},
		{"now", at(0), at(MinThrottle)},
		{"the past", at(-time.Hour), at(MinThrottle)},
		{"zero", &time.Time{}, at(MinThrottle)},
		{"unknown", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := t.TempDir()
			o := Outcome{Status: runner.StatusThrottled, ThrottledUntil: tt.until}
			if err := WriteOutcome(out, o); err != nil {
				t.Fatal(err)
			}
			got, err := ReadOutcome(out, t0)
			if err != nil {
				t.Fatalf("ReadOutcome: %v", err)
			}
			switch {
			case tt.want == nil && got.ThrottledUntil != nil:
				t.Errorf("ThrottledUntil = %v, want nil", got.ThrottledUntil)
			case tt.want != nil && (got.ThrottledUntil == nil || !got.ThrottledUntil.Equal(*tt.want)):
				t.Errorf("ThrottledUntil = %v, want %v", got.ThrottledUntil, tt.want)
			}
		})
	}
	until := t0.Add(time.Hour)
	if got := (Outcome{Status: runner.StatusFailed}).ClampThrottle(t0); got.ThrottledUntil != nil {
		t.Errorf("a failure got a throttle time: %v", got.ThrottledUntil)
	}
	if o := (Outcome{Status: runner.StatusThrottled, ThrottledUntil: &until}); o.ClampThrottle(t0.Add(-7 * time.Hour)).ThrottledUntil.Equal(until) {
		t.Error("the clamp ignored the reader's now")
	}
}
