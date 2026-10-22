package runbundle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// EventVersion is the event stream's version: every event's "v".
const EventVersion = 1

// MaxEventBytes caps one event's line, its newline included.
const MaxEventBytes = 64 << 10

// Event types.
const (
	EventStart    = "start"
	EventStep     = "step"
	EventProgress = "progress"
	EventUsage    = "usage"
	EventOutcome  = "outcome"
)

// Step states.
const (
	StepOK    = "ok"
	StepError = "error"
)

// Event is one line of factory-agent run's stdout. It has one payload: the pointer for its
// Type. An outcome event's Outcome has no Result (OutcomeEvent): the Result is in
// outcome.json.
type Event struct {
	V        int                     `json:"v"`
	Type     string                  `json:"type"`
	At       time.Time               `json:"at"`
	Start    *Start                  `json:"start,omitempty"`
	Step     *Step                   `json:"step,omitempty"`
	Progress *v1alpha1.ProgressEvent `json:"progress,omitempty"`
	Usage    *apiclient.UsageDelta   `json:"usage,omitempty"`
	Outcome  *Outcome                `json:"outcome,omitempty"`
}

// Start is sent once the bundle is read: Version is the bundle's.
type Start struct {
	Version int    `json:"version"`
	RunID   string `json:"runId"`
	TaskID  string `json:"taskId"`
}

// Step is one OnStep call: a pipeline step or an environment sub-step. Error is the step's
// redacted error.
type Step struct {
	Name  string `json:"name"`
	State string `json:"state"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// OutcomeEvent is the outcome event for o: o without its Result, which can be larger than
// an event line and lives in outcome.json (written first).
func OutcomeEvent(o Outcome) Event {
	o.Result = nil
	return Event{Type: EventOutcome, Outcome: &o}
}

// StepOf is the Step for an OnStep call.
func StepOf(name string, d time.Duration, err error) *Step {
	s := &Step{Name: name, State: StepOK, MS: d.Milliseconds()}
	if err != nil {
		s.State, s.Error = StepError, err.Error()
	}
	return s
}

// UsageOf is u in apiclient.UsageDelta's wire shape.
func UsageOf(u runner.UsageDelta) *apiclient.UsageDelta {
	w := &apiclient.UsageDelta{InputTokens: &u.InputTokens, OutputTokens: &u.OutputTokens}
	if u.CostUSD != nil {
		c := float32(*u.CostUSD)
		w.CostUSD = &c
	}
	return w
}

// RunnerUsage is a usage payload as a runner.UsageDelta.
func RunnerUsage(w apiclient.UsageDelta) runner.UsageDelta {
	var u runner.UsageDelta
	if w.InputTokens != nil {
		u.InputTokens = *w.InputTokens
	}
	if w.OutputTokens != nil {
		u.OutputTokens = *w.OutputTokens
	}
	if w.CostUSD != nil {
		c := float64(*w.CostUSD)
		u.CostUSD = &c
	}
	return u
}

// errUnknownType is an event of a type this version doesn't know.
var errUnknownType = errors.New("unknown event type")

// validate checks an event's version, and that it has exactly its type's payload.
func (e Event) validate() error {
	if e.V != EventVersion {
		return invalid("event.v", "%d is not a known version (want %d)", e.V, EventVersion)
	}
	payloads := map[string]bool{
		EventStart: e.Start != nil, EventStep: e.Step != nil, EventProgress: e.Progress != nil,
		EventUsage: e.Usage != nil, EventOutcome: e.Outcome != nil,
	}
	if _, ok := payloads[e.Type]; !ok {
		return errUnknownType
	}
	for t, set := range payloads {
		if set != (t == e.Type) {
			return invalid("event", "a %s event has exactly a %s payload", e.Type, e.Type)
		}
	}
	switch {
	case e.Outcome != nil && e.Outcome.Result != nil:
		return invalid("event.outcome.result", "is in outcome.json only, not in the event")
	case e.Step != nil && e.Step.State != StepOK && e.Step.State != StepError:
		return invalid("event.step.state", "is not ok or error")
	case e.Outcome != nil:
		return e.Outcome.Validate()
	}
	return nil
}

// Encoder writes events as JSON Lines. It's safe for concurrent use: the pipeline's Sink is
// called from several goroutines.
type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewEncoder writes to w; now stamps an event whose At is zero.
func NewEncoder(w io.Writer, now func() time.Time) *Encoder { return &Encoder{w: w, now: now} }

// Encode writes e as one line, its V set to EventVersion. A line over MaxEventBytes is
// refused and nothing is written.
func (enc *Encoder) Encode(e Event) error {
	e.V = EventVersion
	if e.At.IsZero() {
		e.At = enc.now()
	}
	if err := e.validate(); err != nil {
		return fmt.Errorf("event %q: %w", e.Type, err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("event %q: %w", e.Type, err)
	}
	line = append(line, '\n')
	if len(line) > MaxEventBytes {
		return invalid("event", "a %s event is %d bytes, over %d", e.Type, len(line), MaxEventBytes)
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	if _, err := enc.w.Write(line); err != nil {
		return fmt.Errorf("event %q: %w", e.Type, err)
	}
	return nil
}

// Decoder reads the events an untrusted VM wrote.
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder reads from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, MaxEventBytes)}
}

// Next returns the next event, or io.EOF at the end. It skips events of unknown types (a
// newer VM may talk to an older supervisor) and blank lines. A line over MaxEventBytes, one
// that isn't an event, and an event of an unknown version or without its payload are each
// a *ValidationError; the line is consumed, so the caller may go on to the next.
func (d *Decoder) Next() (Event, error) {
	for {
		line, err := d.r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = d.r.ReadSlice('\n')
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return Event{}, fmt.Errorf("events: %w", err)
			}
			return Event{}, invalid("event", "a line is over %d bytes", MaxEventBytes)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return Event{}, fmt.Errorf("events: %w", err)
		}
		if len(line) == 0 && err != nil {
			return Event{}, io.EOF
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return Event{}, io.EOF
			}
			continue
		}
		var e Event
		if jerr := json.Unmarshal(line, &e); jerr != nil {
			return Event{}, invalid("event", "%s", jsonError(jerr))
		}
		switch verr := e.validate(); {
		case errors.Is(verr, errUnknownType):
			continue
		case verr != nil:
			return Event{}, verr
		}
		return e, nil
	}
}
