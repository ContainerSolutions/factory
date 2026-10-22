package runbundle

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The VM's outputs, under the workspace root's OutDir (/work/out).
const (
	OutDir         = "out"
	OutcomeFile    = "outcome.json"
	TranscriptFile = "transcript.jsonl.gz"
	EnvironmentLog = "environment.log"
)

// MaxOutcomeBytes caps outcome.json and an outcome event's payload.
const MaxOutcomeBytes = 2 << 20

// Outcome is runner.Outcome on the wire: outcome.json and the outcome event. Transcript is
// the transcript's file name under OutDir (TranscriptFile), or "" when the run has none.
type Outcome struct {
	Status         string           `json:"status"`
	Reason         string           `json:"reason,omitempty"`
	ThrottledUntil *time.Time       `json:"throttledUntil,omitempty"`
	Result         *v1alpha1.Result `json:"result,omitempty"`
	Transcript     string           `json:"transcript,omitempty"`
}

var (
	statuses = map[string]bool{
		runner.StatusDone: true, runner.StatusFailed: true, runner.StatusNeedsInput: true,
		runner.StatusNoop: true, runner.StatusThrottled: true,
	}
	reasons = map[string]bool{
		"": true, runner.ReasonLimit: true, runner.ReasonInvalidResult: true,
		runner.ReasonHarnessError: true, runner.ReasonStackError: true, runner.ReasonCancelled: true,
	}
)

// Validate checks Status and Reason against the runner's, and Transcript.
func (o Outcome) Validate() error {
	switch {
	case !statuses[o.Status]:
		return invalid("outcome.status", "%q is not a run status", o.Status)
	case !reasons[o.Reason]:
		return invalid("outcome.reason", "%q is not a run reason", o.Reason)
	case o.Transcript != "" && o.Transcript != TranscriptFile:
		return invalid("outcome.transcript", "%q is not %q", o.Transcript, TranscriptFile)
	}
	return nil
}

// OutcomeOf is the wire form of o; transcript is the transcript's file name, or "".
func OutcomeOf(o runner.Outcome, transcript string) Outcome {
	return Outcome{Status: o.Status, Reason: o.Reason, ThrottledUntil: o.ThrottledUntil, Result: o.Result, Transcript: transcript}
}

// Runner is o as a runner.Outcome. TranscriptRef is left empty: the supervisor uploads the
// transcript and sets it.
func (o Outcome) Runner() runner.Outcome {
	return runner.Outcome{Status: o.Status, Reason: o.Reason, ThrottledUntil: o.ThrottledUntil, Result: o.Result}
}

// WriteOutcome validates o and writes it to outDir/OutcomeFile through a temp file and a
// rename, creating outDir (0700) when needed.
func WriteOutcome(outDir string, o Outcome) error {
	if err := o.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	if len(data) > MaxOutcomeBytes {
		return invalid(OutcomeFile, "is over %d bytes", MaxOutcomeBytes)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	tmp, err := os.CreateTemp(outDir, OutcomeFile+".*")
	if err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(outDir, OutcomeFile))
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("outcome: %w", err)
	}
	return nil
}

// ReadOutcome reads outDir/OutcomeFile, written by the untrusted VM: at most MaxOutcomeBytes,
// and validated.
func ReadOutcome(outDir string) (Outcome, error) {
	name := filepath.Join(outDir, OutcomeFile)
	fi, err := os.Lstat(name)
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Outcome{}, invalid(OutcomeFile, "is not a regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxOutcomeBytes+1))
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	if len(data) > MaxOutcomeBytes {
		return Outcome{}, invalid(OutcomeFile, "is over %d bytes", MaxOutcomeBytes)
	}
	var o Outcome
	if err := json.Unmarshal(data, &o); err != nil {
		return Outcome{}, invalid(OutcomeFile, "%v", err)
	}
	if err := o.Validate(); err != nil {
		return Outcome{}, err
	}
	return o, nil
}
