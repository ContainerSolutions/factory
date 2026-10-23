package vmrun

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is closed") }

// An event that can't be written (stdout gone, a line over the cap) is logged and dropped:
// the run goes on, and its outcome is in outcome.json.
func TestEventsDropUnwritable(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return t0 }
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	(&events{enc: runbundle.NewEncoder(failWriter{}, now), log: log}).Progress(v1alpha1.ProgressEvent{At: t0, Kind: "message", Message: "hi"})
	if !strings.Contains(logs.String(), "an event wasn't sent") || !strings.Contains(logs.String(), "stdout is closed") {
		t.Errorf("log %q", logs.String())
	}

	logs.Reset()
	var out bytes.Buffer
	e := &events{enc: runbundle.NewEncoder(&out, now), log: log}
	e.Progress(v1alpha1.ProgressEvent{At: t0, Kind: "message", Message: strings.Repeat("x", runbundle.MaxEventBytes)})
	e.step("clone", time.Second, nil)
	if !strings.Contains(logs.String(), "type=progress") || strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"name":"clone"`) {
		t.Errorf("out %q, log %q", out.String(), logs.String())
	}
}
