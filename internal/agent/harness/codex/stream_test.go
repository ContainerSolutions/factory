package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the parser goldens under testdata/")

// t0 is the fake clock's time in the stream tests.
var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// parsed is a golden: what the parser reported for one recorded stream.
type parsed struct {
	Progress    []goldenEvent        `json:"progress"`
	Usage       []harness.UsageDelta `json:"usage"`
	RateLimited []time.Time          `json:"rateLimited"`
	ThreadID    string               `json:"threadId"`
	Turns       int                  `json:"turns"`
	LastMessage string               `json:"lastMessage"`
	Failed      bool                 `json:"failed"`
	LastError   string               `json:"lastError"`
}

type goldenEvent struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

func parseFile(t *testing.T, path string) parsed {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &harnesstest.Recorder{}
	p := newParser(s, clock.NewFake(t0), DefaultThrottle)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	out := parsed{Usage: s.Usages, RateLimited: s.Limits, ThreadID: p.threadID, Turns: p.turns,
		LastMessage: p.lastMessage, Failed: p.failed, LastError: p.lastError}
	for _, e := range s.Events {
		if !e.At.Equal(t0) {
			t.Errorf("event %+v isn't stamped with the clock", e)
		}
		out.Progress = append(out.Progress, goldenEvent{Kind: e.Kind, Summary: e.Summary})
	}
	return out
}

// TestParseRecordedStreams replays every testdata/codex-*.jsonl through the parser and
// compares what it reported with the .golden.json next to it (-update rewrites them).
func TestParseRecordedStreams(t *testing.T) {
	files, err := filepath.Glob("testdata/codex-*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded streams: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			got, err := json.MarshalIndent(parseFile(t, f), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".jsonl") + ".golden.json"
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from %s:\n%s", f, golden, got)
			}
		})
	}
}

// TestParseSemantics pins the goldens' meaning, so -update can't silently bless a regression.
func TestParseSemantics(t *testing.T) {
	kinds := func(p parsed, kind string) int {
		n := 0
		for _, e := range p.Progress {
			if e.Kind == kind {
				n++
			}
		}
		return n
	}
	sum := func(p parsed) (in, out int64) {
		for _, u := range p.Usage {
			if u.CostUSD != nil {
				t.Errorf("a codex usage delta has a cost: %+v", u)
			}
			in, out = in+u.InputTokens, out+u.OutputTokens
		}
		return in, out
	}

	ok := parseFile(t, "testdata/codex-success.jsonl")
	if ok.ThreadID != "0199f2a1-6b1e-7c33-9a51-2f0c4e8d1a01" || ok.Turns != 1 || kinds(ok, harness.KindTurn) != 1 ||
		kinds(ok, harness.KindTool) != 3 || kinds(ok, harness.KindMessage) != 1 || ok.Failed || len(ok.RateLimited) != 0 {
		t.Errorf("success: %+v", ok)
	}
	if in, out := sum(ok); in != 24763 || out != 1220 {
		t.Errorf("success usage %d/%d", in, out)
	}
	if !strings.Contains(ok.LastMessage, `"outcome":"pr-opened"`) {
		t.Errorf("success last message %q", ok.LastMessage)
	}

	multi := parseFile(t, "testdata/codex-multiturn.jsonl")
	if multi.Turns != 2 || kinds(multi, harness.KindTurn) != 2 || kinds(multi, harness.KindTool) != 3 || kinds(multi, harness.KindError) != 1 {
		t.Errorf("multi-turn: %+v", multi)
	}
	if in, out := sum(multi); in != 39000 || out != 1350 {
		t.Errorf("multi-turn usage %d/%d", in, out)
	}

	rl := parseFile(t, "testdata/codex-ratelimit.jsonl")
	if len(rl.RateLimited) != 1 || !rl.RateLimited[0].Equal(t0.Add(2*time.Hour+5*time.Minute)) || !rl.Failed {
		t.Errorf("usage limit: %+v", rl)
	}
	r429 := parseFile(t, "testdata/codex-ratelimit-429.jsonl")
	if len(r429.RateLimited) != 1 || !r429.RateLimited[0].Equal(t0.Add(DefaultThrottle)) {
		t.Errorf("429: %+v", r429)
	}
	reset := parseFile(t, "testdata/codex-ratelimit-reset.jsonl")
	if len(reset.RateLimited) != 1 || !reset.RateLimited[0].Equal(t0.Add(90*time.Minute)) {
		t.Errorf("resets_in_seconds: %+v", reset)
	}

	bad := parseFile(t, "testdata/codex-error.jsonl")
	if !bad.Failed || len(bad.RateLimited) != 0 || kinds(bad, harness.KindError) != 3 || !strings.Contains(bad.LastError, "stream disconnected") {
		t.Errorf("error: %+v", bad)
	}

	legacy := parseFile(t, "testdata/codex-token-count.jsonl")
	if in, out := sum(legacy); in != 12000 || out != 700 {
		t.Errorf("token_count usage %d/%d", in, out)
	}
}

func TestParserEdgeCases(t *testing.T) {
	s := &harnesstest.Recorder{}
	p := newParser(s, clock.NewFake(t0), DefaultThrottle)
	long := strings.Repeat("é", 400) // 800 bytes
	for _, l := range []string{
		"", "not json", "[1,2]", `{"type":`,
		`{"type":"item.completed","item":{"id":"a","type":"agent_message","text":"` + long + `"}}`,
		// usage and turn.completed: token_count is then ignored
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`,
		`{"type":"token_count","info":{"last_token_usage":{"input_tokens":99,"output_tokens":99}}}`,
		`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`,
		// an error object given as a string, then a second limit: reported once
		`{"type":"turn.failed","error":"Rate limit reached for gpt-5-codex"}`,
		`{"type":"error","message":"usage limit again"}`,
	} {
		p.line([]byte(l))
	}
	if len(s.Events) != 3 || len(s.Events[0].Summary) > harness.MaxSummary || !strings.HasPrefix(long, s.Events[0].Summary) {
		t.Errorf("progress %+v", s.Events)
	}
	if len(s.Usages) != 1 || s.Usages[0].InputTokens != 10 {
		t.Errorf("usage %+v", s.Usages)
	}
	if len(s.Limits) != 1 || p.lastError != "usage limit again" {
		t.Errorf("limits %v, last error %q", s.Limits, p.lastError)
	}
}

func TestIsRateLimit(t *testing.T) {
	for msg, want := range map[string]bool{
		"You've hit your usage limit.":                             true,
		"exceeded retry limit, last status: 429 Too Many Requests": true,
		"Rate limit reached for requests":                          true,
		"insufficient_quota":                                       true,
		"stream disconnected before completion":                    false,
		"command failed with exit code 1":                          false,
	} {
		if got := isRateLimit(msg); got != want {
			t.Errorf("isRateLimit(%q) = %v", msg, got)
		}
	}
}
