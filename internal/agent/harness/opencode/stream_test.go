package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// t0 is the fake clock's time in the stream and run tests.
var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// TestParseStreams replays every testdata/opencode-*.jsonl through the parser and compares
// what it reported with the .golden.json next to it (-update rewrites them).
func TestParseStreams(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/opencode-*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no streams: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := &harnesstest.Recorder{}
		p := newParser(s, clock.NewFake(t0), DefaultThrottle)
		for l := range bytes.Lines(b) {
			p.line(l)
		}
		got, err := json.MarshalIndent(map[string]any{"progress": s.Events, "usage": s.Usages, "rateLimited": s.Limits,
			"sessionId": p.sessionID, "lastText": p.lastText, "lastError": p.lastError, "recovered": p.recovered}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden(t, harness.Workspace{Root: t.TempDir()}, strings.TrimSuffix(filepath.Base(f), ".jsonl")+".golden.json", append(got, '\n'))
	}
}
