package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/clock"
)

// parser turns `codex exec --json` lines into the EventSink's progress, usage and rate
// limits (harness-adapters.md §Codex, Stream). Each turn.started is one turn; tool items
// (commands, file changes, MCP calls, web searches) are tool events, agent messages message
// events, and errors error events. Usage comes from turn.completed (older streams:
// token_count). An error with rate- or usage-limit wording reports RateLimited once, with the
// reset when the error gives one, else now + DefaultThrottle. It is not safe for concurrent
// use: one goroutine reads one process's stdout.
type parser struct {
	sink  harness.EventSink
	clk   clock.Clock
	reset time.Duration // the throttle when no reset is given

	threadID    string
	turns       int
	lastMessage string
	lastError   string
	failed      bool // a turn failed
	limited     bool
	until       time.Time
	tools       map[string]bool // item ids already reported
	sawTurnUse  bool            // turn.completed carried usage: token_count is ignored
}

func newParser(sink harness.EventSink, clk clock.Clock, reset time.Duration) *parser {
	return &parser{sink: sink, clk: clk, reset: reset, tools: map[string]bool{}}
}

// event is one JSONL line: the fields of every event type the parser reads.
type event struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Usage    *tokenUsage     `json:"usage"`
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
	Item     *item           `json:"item"`
	Info     *struct {
		Last *tokenUsage `json:"last_token_usage"`
	} `json:"info"`
	// Msg is the older envelope ({"id": …, "msg": {"type": …}}).
	Msg json.RawMessage `json:"msg"`
}

type tokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

type item struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Message  string `json:"message"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	Status   string `json:"status"`
	Server   string `json:"server"`
	Tool     string `json:"tool"`
	Query    string `json:"query"`
	Changes  []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`
}

// line parses one stdout line; a line that isn't a JSON object is ignored (it is still in
// the transcript).
func (p *parser) line(b []byte) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return
	}
	var ev event
	if err := json.Unmarshal(b, &ev); err != nil {
		return
	}
	if ev.Type == "" && len(ev.Msg) > 0 {
		var inner event
		if err := json.Unmarshal(ev.Msg, &inner); err == nil {
			ev = inner
		}
	}
	p.handle(ev, b)
}

func (p *parser) handle(ev event, raw []byte) {
	now := p.clk.Now()
	switch ev.Type {
	case evThreadStarted:
		if ev.ThreadID != "" {
			p.threadID = ev.ThreadID
		}
	case evTurnStarted:
		p.turns++
		p.progress(harness.KindTurn, fmt.Sprintf("turn %d", p.turns), now)
	case evTurnCompleted:
		if ev.Usage != nil {
			p.sawTurnUse = true
			p.usage(*ev.Usage)
		}
	case evTokenCount:
		if !p.sawTurnUse && ev.Info != nil && ev.Info.Last != nil {
			p.usage(*ev.Info.Last)
		}
	case evTurnFailed:
		p.failed = true
		p.fail(errorMessage(ev.Error, ev.Message), raw, now)
	case evError:
		// Not necessarily fatal (codex reports retries this way too): the exit status says.
		p.fail(errorMessage(ev.Error, ev.Message), raw, now)
	case evItemStarted, evItemUpdated, evItemCompleted:
		if ev.Item != nil {
			p.item(ev.Type, *ev.Item, raw, now)
		}
	}
}

func (p *parser) item(typ string, it item, raw []byte, now time.Time) {
	switch it.Type {
	case itemAgentMessage:
		if typ == evItemCompleted && it.Text != "" {
			p.lastMessage = it.Text
			p.progress(harness.KindMessage, clirun.FirstLine(it.Text), now)
		}
	case itemCommand, itemFileChange, itemMCPToolCall, itemWebSearch:
		// One event per tool call: when it starts, or when it completes if its start
		// wasn't streamed.
		if typ == evItemUpdated || p.tools[it.ID] && it.ID != "" {
			return
		}
		if it.ID != "" {
			p.tools[it.ID] = true
		}
		p.progress(harness.KindTool, toolSummary(it), now)
	case itemError:
		if typ == evItemCompleted {
			msg := it.Message
			if msg == "" {
				msg = it.Text
			}
			p.fail(msg, raw, now)
		}
	case itemTodoList:
		if typ != evItemUpdated && len(it.Items) > 0 {
			done := 0
			for _, t := range it.Items {
				if t.Completed {
					done++
				}
			}
			p.progress(harness.KindMessage, fmt.Sprintf("plan: %d/%d done", done, len(it.Items)), now)
		}
	case itemReasoning:
		// Transcript only.
	}
}

// fail reports an error and checks it for a rate or plan limit.
func (p *parser) fail(msg string, raw []byte, now time.Time) {
	if msg == "" {
		msg = "codex reported an error"
	}
	p.lastError = msg
	p.progress(harness.KindError, clirun.FirstLine(msg), now)
	if p.limited || !isRateLimit(msg) {
		return
	}
	p.limited = true
	p.until = clirun.ResetTime(raw, msg, now, p.reset)
	p.sink.RateLimited(p.until)
}

func (p *parser) usage(u tokenUsage) {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	// Subscription runs have no cost; API-key runs aren't priced by the stream either.
	p.sink.Usage(harness.UsageDelta{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
}

func (p *parser) progress(kind, summary string, at time.Time) {
	p.sink.Progress(harness.ProgressEvent{Kind: kind, Summary: clirun.Clip(summary, harness.MaxSummary), At: at})
}

// errorMessage is an error event's text: error.message, error as a string, or message.
func errorMessage(e json.RawMessage, msg string) string {
	if len(e) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
		var s string
		if json.Unmarshal(e, &s) == nil && s != "" {
			return s
		}
	}
	return msg
}

func toolSummary(it item) string {
	switch it.Type {
	case itemCommand:
		return "$ " + clirun.FirstLine(it.Command)
	case itemFileChange:
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		return "edit " + strings.Join(paths, ", ")
	case itemMCPToolCall:
		return "mcp " + it.Server + "." + it.Tool
	case itemWebSearch:
		return "search " + clirun.FirstLine(it.Query)
	}
	return it.Type
}

// isRateLimit reports whether an error's text says a rate or plan limit was hit.
func isRateLimit(msg string) bool { return clirun.ContainsFold(msg, rateLimitWords) }
