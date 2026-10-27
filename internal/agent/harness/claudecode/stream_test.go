package claudecode

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
	_ "time/tzdata" // the reset zones in the recorded streams, wherever the tests run

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/ (the parser's and the VM files)")

// t0 is the fake clock's time in the stream tests.
var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// parsed is a golden: what the parser reported for one recorded stream.
type parsed struct {
	Progress    []goldenEvent        `json:"progress"`
	Usage       []harness.UsageDelta `json:"usage"`
	RateLimited []time.Time          `json:"rateLimited"`
	SessionID   string               `json:"sessionId"`
	Turns       int                  `json:"turns"`
	Result      string               `json:"result"`
	Failed      bool                 `json:"failed"`
	LastError   string               `json:"lastError"`
	Limit       string               `json:"limit"`
	Fatal       string               `json:"fatal"`
	Stopped     bool                 `json:"stopped"`
}

type goldenEvent struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

func parseFile(t *testing.T, path string, apiKey bool) parsed {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &harnesstest.Recorder{}
	p := newParser(s, clock.NewFake(t0), DefaultThrottle, apiKey)
	stopped := false
	p.onFatal = func() { stopped = true }
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	for i, l := range s.Limits {
		s.Limits[i] = l.UTC() // goldens don't depend on the machine's zone
	}
	out := parsed{Usage: s.Usages, RateLimited: s.Limits, SessionID: p.sessionID, Turns: p.turns, Result: string(p.finalResult()),
		Failed: p.failed, LastError: p.lastError, Fatal: p.fatal, Stopped: stopped}
	if p.limit != nil {
		out.Limit = p.limit.Limit
	}
	for _, e := range s.Events {
		if !e.At.Equal(t0) {
			t.Errorf("event %+v isn't stamped with the clock", e)
		}
		out.Progress = append(out.Progress, goldenEvent{Kind: e.Kind, Summary: e.Summary})
	}
	return out
}

// apiKeyStreams are parsed as API-key runs (their cost counts).
var apiKeyStreams = map[string]bool{"claude-ratelimit-429.jsonl": true, "claude-multiturn.jsonl": true}

// TestParseRecordedStreams replays every testdata/claude-*.jsonl through the parser and
// compares what it reported with the .golden.json next to it (-update rewrites them).
func TestParseRecordedStreams(t *testing.T) {
	files, err := filepath.Glob("testdata/claude-*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded streams: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			got, err := json.MarshalIndent(parseFile(t, f, apiKeyStreams[filepath.Base(f)]), "", "  ")
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
	sum := func(p parsed) (in, out int64, cost *float64) {
		for _, u := range p.Usage {
			in, out = in+u.InputTokens, out+u.OutputTokens
			if u.CostUSD != nil {
				cost = u.CostUSD
			}
		}
		return in, out, cost
	}

	ok := parseFile(t, "testdata/claude-success.jsonl", false)
	if ok.SessionID != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" || ok.Turns != 5 || kinds(ok, harness.KindTurn) != 5 ||
		kinds(ok, harness.KindTool) != 4 || kinds(ok, harness.KindMessage) != 2 || kinds(ok, harness.KindError) != 0 ||
		ok.Failed || len(ok.RateLimited) != 0 || ok.Limit != "" {
		t.Errorf("success: %+v", ok)
	}
	if in, out, cost := sum(ok); in != 62215 || out != 510 || cost != nil {
		t.Errorf("success usage %d/%d, cost %v (a subscription run has none)", in, out, cost)
	}
	if ok.Result != `{"status":"done","outcome":"pr-opened","summary":"opened acme/docs#13","data":{"prs":["acme/docs#13"]}}` {
		t.Errorf("success result %q", ok.Result)
	}
	if in, out, cost := sum(parseFile(t, "testdata/claude-success.jsonl", true)); in != 62215 || out != 510 || cost == nil || *cost != 0.1834 {
		t.Errorf("success on an API key: %d/%d, cost %v", in, out, cost)
	}

	multi := parseFile(t, "testdata/claude-multiturn.jsonl", true)
	if multi.Turns != 4 || kinds(multi, harness.KindTurn) != 4 || kinds(multi, harness.KindTool) != 3 || kinds(multi, harness.KindError) != 2 || multi.Failed {
		t.Errorf("multi-turn: %+v", multi)
	}
	if in, out, cost := sum(multi); in != 28102 || out != 340 || cost == nil || *cost != 0.09 {
		t.Errorf("multi-turn usage %d/%d, cost %v: the result's totals top the streamed usage up", in, out, cost)
	}
	if multi.Result != `{"status":"done","outcome":"decomposed","summary":"five sub-issues"}` {
		t.Errorf("multi-turn result (unfenced) %q", multi.Result)
	}

	rl := parseFile(t, "testdata/claude-ratelimit.jsonl", false)
	if len(rl.RateLimited) != 1 || !rl.RateLimited[0].Equal(t0.Add(2*time.Hour+5*time.Minute)) || !rl.Failed || rl.Turns != 1 {
		t.Errorf("usage limit: %+v", rl)
	}
	resets := parseFile(t, "testdata/claude-ratelimit-resets.jsonl", false)
	if len(resets.RateLimited) != 1 || !resets.RateLimited[0].Equal(t0.Add(2*time.Hour)) { // 3pm in Lisbon is 14:00 UTC
		t.Errorf("resets 3pm: %+v", resets)
	}
	r429 := parseFile(t, "testdata/claude-ratelimit-429.jsonl", true)
	if len(r429.RateLimited) != 1 || !r429.RateLimited[0].Equal(t0.Add(DefaultThrottle)) {
		t.Errorf("429: %+v", r429)
	}
	ev := parseFile(t, "testdata/claude-ratelimit-event.jsonl", false)
	if len(ev.RateLimited) != 1 || !ev.RateLimited[0].Equal(t0.Add(90*time.Minute)) {
		t.Errorf("rate_limit_event: %+v", ev)
	}

	bad := parseFile(t, "testdata/claude-error.jsonl", false)
	if !bad.Failed || len(bad.RateLimited) != 0 || !strings.Contains(bad.LastError, "Connection error.") {
		t.Errorf("error: %+v", bad)
	}
	turns := parseFile(t, "testdata/claude-maxturns.jsonl", false)
	if turns.Limit != harness.LimitTurns || turns.Failed {
		t.Errorf("max turns: %+v", turns)
	}
	plugin := parseFile(t, "testdata/claude-plugin-error.jsonl", false)
	if !plugin.Stopped || !strings.Contains(plugin.Fatal, "Invalid manifest") {
		t.Errorf("plugin error: %+v", plugin)
	}
}

func TestParserEdgeCases(t *testing.T) {
	s := &harnesstest.Recorder{}
	p := newParser(s, clock.NewFake(t0), DefaultThrottle, false)
	long := strings.Repeat("é", 400) // 800 bytes
	for _, l := range []string{
		"", "not json", "[1,2]", `{"type":`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"` + long + `"}]},"parent_tool_use_id":null}`,
		// a string content, the same tool_use twice, a message without id
		`{"type":"assistant","message":{"id":"m2","content":"plain text"}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"ccf-docs:docs-style"}}]}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"ccf-docs:docs-style"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"NotebookEdit","input":{"notebook_path":"/w/n.ipynb"}}],"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"WebSearch","input":{"query":"go 1.26"}}],"usage":{"input_tokens":5,"output_tokens":1}}}`,
		// two rate-limit refusals: reported once
		`{"type":"assistant","message":{"id":"x","content":[{"type":"text","text":"API Error: Rate limit reached"}]},"error":"rate_limit"}`,
		`{"type":"result","subtype":"success","is_error":true,"result":"usage limit again"}`,
	} {
		p.line([]byte(l))
	}
	var summaries []string
	for _, e := range s.Events {
		summaries = append(summaries, e.Kind+":"+e.Summary)
	}
	if len(s.Events[1].Summary) > harness.MaxSummary || !strings.HasPrefix(long, s.Events[1].Summary) {
		t.Errorf("a long message isn't clipped: %d bytes", len(s.Events[1].Summary))
	}
	want := []string{"turn:turn 2", "message:plain text", "tool:skill ccf-docs:docs-style", "turn:turn 3", "tool:edit /w/n.ipynb", "turn:turn 4", "tool:search go 1.26",
		"error:API Error: Rate limit reached", "error:usage limit again"}
	if got := summaries[2:]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("progress\n  %q\nwant\n  %q", got, want)
	}
	if len(s.Usages) != 2 || s.Usages[1].InputTokens != 5 {
		t.Errorf("usage %+v: messages without an id are each counted", s.Usages)
	}
	if len(s.Limits) != 1 || p.lastError != "usage limit again" || !p.failed {
		t.Errorf("limits %v, last error %q", s.Limits, p.lastError)
	}
}

func TestToolSummary(t *testing.T) {
	for name, want := range map[string]string{
		"mcp__factory__pr_open": "mcp factory.pr_open",
		"Read":                  "read /w/a",
		"Glob":                  "search *.go",
		"Task":                  "agent look",
		"WebFetch":              "fetch https://x",
		"StructuredOutput":      "result",
		"Mystery":               "Mystery",
	} {
		if got := toolSummary(name, toolInput{FilePath: "/w/a", Pattern: "*.go", Description: "look", URL: "https://x"}); got != want {
			t.Errorf("toolSummary(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestIsRateLimit(t *testing.T) {
	for msg, want := range map[string]bool{
		"Claude AI usage limit reached|1792418700":           true,
		"5-hour limit reached ∙ resets 3pm":                  true,
		"You've hit your limit · resets Oct 21, 9am":         true,
		`API Error: 429 {"type":"rate_limit_error"}`:         true,
		"API Error: 500 Internal server error":               false,
		"Invalid API key · Please run /login":                false,
		"Reached maximum number of turns (3)":                false,
		"stream ended without a message_stop":                false,
		"Credit balance is too low to access the Claude API": false,
	} {
		if got := isRateLimit(msg); got != want {
			t.Errorf("isRateLimit(%q) = %v", msg, got)
		}
	}
}

// TestDecodeStringFields: only a top-level string that parses as a JSON object or array is
// decoded.
func TestDecodeStringFields(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"a stringified object", `{"status":"done","data":"{}"}`, `{"data":{},"status":"done"}`},
		{"a stringified array, spaces around", `{"artifacts":" [{\"kind\":\"pr\"}]\n"}`, `{"artifacts":[{"kind":"pr"}]}`},
		{"a string that isn't JSON", `{"data":"2 findings"}`, `{"data":"2 findings"}`},
		{"a string that starts like JSON but isn't", `{"summary":"[wip] {not json"}`, `{"summary":"[wip] {not json"}`},
		{"a JSON scalar in a string", `{"data":"42"}`, `{"data":"42"}`},
		{"a nested string untouched", `{"data":{"x":"{}"}}`, `{"data":{"x":"{}"}}`},
		{"not an object", `["{}"]`, `["{}"]`},
		{"not JSON", `{"data":`, `{"data":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(decodeStringFields([]byte(tt.in))); got != tt.want {
				t.Errorf("decodeStringFields(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
