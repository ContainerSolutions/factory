package runbundle

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// The VM's outputs, under the workspace root's OutDir (/work/out).
const (
	OutDir         = "out"
	OutcomeFile    = "outcome.json"
	TranscriptFile = "transcript.jsonl.gz"
	EnvironmentLog = "environment.log"
)

// MaxOutcomeBytes caps outcome.json. The outcome event carries the Outcome without its
// Result (see OutcomeEvent), so it fits an event line.
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

// MaxThrottle bounds a VM's throttle: ReadOutcome and ClampThrottle move a later
// ThrottledUntil to now + MaxThrottle, so one VM can't park the agent host. A longer real
// limit (codex's weekly cap) throttles again each window.
const MaxThrottle = 6 * time.Hour

// MinThrottle is the throttle ClampThrottle gives a throttled outcome whose time is zero or
// not after now.
const MinThrottle = time.Minute

// Validate checks Status and Reason against runner.Statuses and runner.Reasons, that
// ThrottledUntil is set only for a throttled outcome, and Transcript. Its errors never quote a
// value: an outcome comes from the untrusted VM.
func (o Outcome) Validate() error {
	switch {
	case !slices.Contains(runner.Statuses(), o.Status):
		return invalid("outcome.status", "is not a run status")
	case o.Reason != "" && !slices.Contains(runner.Reasons(), o.Reason):
		return invalid("outcome.reason", "is not a run reason")
	case o.ThrottledUntil != nil && o.Status != runner.StatusThrottled:
		return invalid("outcome.throttledUntil", "is set only when the status is throttled")
	case o.Transcript != "" && o.Transcript != TranscriptFile:
		return invalid("outcome.transcript", "is not %q", TranscriptFile)
	}
	return nil
}

// ClampThrottle bounds a throttled outcome's ThrottledUntil by the reader's now: a time after
// now + MaxThrottle becomes that, and a zero time or one not after now becomes
// now + MinThrottle. A nil ThrottledUntil (unknown) stays nil: the worker applies its
// default. ReadOutcome applies it; the supervisor applies it to an outcome event too.
func (o Outcome) ClampThrottle(now time.Time) Outcome {
	if o.Status != runner.StatusThrottled || o.ThrottledUntil == nil {
		return o
	}
	until := *o.ThrottledUntil
	switch limit := now.Add(MaxThrottle); {
	case until.IsZero() || !until.After(now):
		until = now.Add(MinThrottle)
	case until.After(limit):
		until = limit
	}
	o.ThrottledUntil = &until
	return o
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

// ReadOutcome reads outDir/OutcomeFile, written by the untrusted VM: outDir must be a real
// directory (not a symlink), and the file a regular file with one link (no symlink, FIFO,
// device or second link, checked before and after opening it, and opened non-blocking), at most
// MaxOutcomeBytes and valid. Its ThrottledUntil is clamped by now (ClampThrottle).
func ReadOutcome(outDir string, now time.Time) (Outcome, error) {
	dirInfo, err := os.Lstat(outDir)
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	if !dirInfo.IsDir() {
		return Outcome{}, invalid(OutDir, "is not a directory")
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	defer func() { _ = root.Close() }()
	if opened, err := root.Stat("."); err != nil || !os.SameFile(dirInfo, opened) {
		return Outcome{}, invalid(OutDir, "changed while it was opened")
	}
	before, err := root.Lstat(OutcomeFile)
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	if !before.Mode().IsRegular() {
		return Outcome{}, invalid(OutcomeFile, "is not a regular file")
	}
	f, after, err := openRegular(root, OutcomeFile)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = f.Close() }()
	if !os.SameFile(before, after) {
		return Outcome{}, invalid(OutcomeFile, "changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxOutcomeBytes+1))
	if err != nil {
		return Outcome{}, fmt.Errorf("outcome: %w", err)
	}
	if len(data) > MaxOutcomeBytes {
		return Outcome{}, invalid(OutcomeFile, "is over %d bytes", MaxOutcomeBytes)
	}
	var o Outcome
	if err := json.Unmarshal(data, &o); err != nil {
		return Outcome{}, invalid(OutcomeFile, "%s", jsonError(err))
	}
	if err := o.Validate(); err != nil {
		return Outcome{}, err
	}
	return o.ClampThrottle(now), nil
}

// openRegular opens name in root without blocking (a FIFO opens at once) and returns it only
// when the opened file is a regular file with a single link.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("outcome: %w", err)
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
		_ = f.Close()
		return nil, nil, fmt.Errorf("outcome: %w", err)
	case !fi.Mode().IsRegular():
		_ = f.Close()
		return nil, nil, invalid(name, "is not a regular file")
	case links(fi) > 1:
		_ = f.Close()
		return nil, nil, invalid(name, "has more than one link")
	}
	return f, fi, nil
}
