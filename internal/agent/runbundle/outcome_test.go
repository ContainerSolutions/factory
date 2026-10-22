package runbundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
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
			got, err := ReadOutcome(out)
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
		{"an oversize outcome", `{"status":"done","result":{"summary":"` + strings.Repeat("a", MaxOutcomeBytes) + `"}}`, OutcomeFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := t.TempDir()
			if err := os.WriteFile(filepath.Join(out, OutcomeFile), []byte(tt.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ReadOutcome(out)
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
		_, err := ReadOutcome(out)
		wantInvalid(t, err, OutcomeFile)
	})

	t.Run("WriteOutcome", func(t *testing.T) {
		out := t.TempDir()
		wantInvalid(t, WriteOutcome(out, Outcome{Status: "great"}), "outcome.status")
		big := Outcome{Status: runner.StatusDone, Result: &v1alpha1.Result{Summary: strings.Repeat("a", MaxOutcomeBytes)}}
		wantInvalid(t, WriteOutcome(out, big), OutcomeFile)
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Errorf("out/ has %d entries", len(entries))
		}
	})
}
