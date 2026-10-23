package claudecode

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

// parser turns `claude -p --output-format stream-json --verbose` lines into the EventSink's
// progress, usage and rate limits (harness-adapters.md §Claude Code, Stream):
//   - system/init records the session; a plugin error is fatal (the run is stopped), an MCP
//     server that didn't connect an error event;
//   - each new top-level assistant message is one turn; its text a message event, each
//     tool_use a tool event (subagents' tools too), TodoWrite a "plan: n/m done" message;
//   - usage is streamed per message (input including the cached tokens), and the result's
//     totals top it up; the cost counts only for API-key runs;
//   - the result says how the run ended: a limit (max turns, budget), a failure, the final
//     text and the structured output;
//   - a rate_limit_event that rejected the run, an assistant message refused for the rate
//     limit, or an error result with rate- or usage-limit wording reports RateLimited once,
//     with the reset when it gives one, else now + DefaultThrottle.
//
// It is not safe for concurrent use: one goroutine reads one process's stdout.
type parser struct {
	sink   harness.EventSink
	clk    clock.Clock
	reset  time.Duration // the throttle when no reset is given
	apiKey bool          // the run's cost counts
	// onFatal stops the process (a plugin failed to load).
	onFatal func()

	sessionID  string
	turns      int
	messages   map[string]tokens // usage seen per message id
	streamed   tokens            // usage reported so far
	tools      map[string]bool   // tool_use ids reported
	lastText   string            // the last top-level text
	resultText string
	structured json.RawMessage
	// structuredFailed: claude ran out of StructuredOutput retries (and exits 1); structured
	// is then the last refused attempt.
	structuredFailed bool
	failed           bool
	lastError        string
	limited          bool
	until            time.Time
	limit            *harness.LimitError
	fatal            string
}

type tokens struct{ in, out int64 }

func newParser(sink harness.EventSink, clk clock.Clock, reset time.Duration, apiKey bool) *parser {
	return &parser{sink: sink, clk: clk, reset: reset, apiKey: apiKey, messages: map[string]tokens{}, tools: map[string]bool{}}
}

// event is one stream-json line: the fields of every event type the parser reads.
type event struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	// assistant / user
	Message         *message `json:"message"`
	ParentToolUseID *string  `json:"parent_tool_use_id"`
	Error           string   `json:"error"`
	// system/init
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	PluginErrors json.RawMessage `json:"plugin_errors"`
	// result
	IsError           bool            `json:"is_error"`
	Result            string          `json:"result"`
	Errors            []string        `json:"errors"`
	NumTurns          int             `json:"num_turns"`
	TotalCostUSD      *float64        `json:"total_cost_usd"`
	Usage             *tokenUsage     `json:"usage"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
	// rate_limit_event
	RateLimitInfo *struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

type message struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
	Usage   *tokenUsage     `json:"usage"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type tokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// tokens counts the cached input as input (as the codex adapter does).
func (u tokenUsage) tokens() tokens {
	return tokens{in: u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens, out: u.OutputTokens}
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
	if ev.SessionID != "" {
		p.sessionID = ev.SessionID
	}
	now := p.clk.Now()
	switch ev.Type {
	case evSystem:
		if ev.Subtype == subInit {
			p.init(ev, now)
		}
	case evAssistant:
		p.assistant(ev, b, now)
	case evResult:
		p.result(ev, b, now)
	case evRateLimit:
		p.rateLimitEvent(ev, b, now)
	case evUser:
		// Tool results: transcript only.
	}
}

func (p *parser) init(ev event, now time.Time) {
	for _, s := range ev.MCPServers {
		if s.Status != mcpConnected {
			p.progress(harness.KindError, fmt.Sprintf("MCP server %s: %s", s.Name, s.Status), now)
		}
	}
	if hasItems(ev.PluginErrors) {
		p.fatal = "plugins failed to load: " + clirun.Clip(string(ev.PluginErrors), maxDetail)
		p.lastError = p.fatal
		p.progress(harness.KindError, p.fatal, now)
		if p.onFatal != nil {
			p.onFatal()
		}
	}
}

func (p *parser) assistant(ev event, raw []byte, now time.Time) {
	m := ev.Message
	if m == nil {
		return
	}
	top := ev.ParentToolUseID == nil || *ev.ParentToolUseID == ""
	var blocks []block
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			blocks = []block{{Type: blockText, Text: s}}
		}
	}
	if ev.Error != "" {
		// The API refused the turn; its text is the error.
		var text []string
		for _, b := range blocks {
			if b.Type == blockText {
				text = append(text, b.Text)
			}
		}
		msg := strings.TrimSpace(strings.Join(text, "\n"))
		if msg == "" {
			msg = ev.Error
		}
		p.fail(msg, raw, now, ev.Error == assistantErrRateLimit)
		return
	}
	prev, seen := p.messages[m.ID]
	if m.ID == "" {
		prev, seen = tokens{}, false
	}
	if !seen {
		p.messages[m.ID] = tokens{}
		if top {
			p.turns++
			p.progress(harness.KindTurn, fmt.Sprintf("turn %d", p.turns), now)
		}
	}
	if m.Usage != nil {
		// A message's usage is repeated (and may grow) on each of its content blocks' events.
		u := m.Usage.tokens()
		cur := tokens{in: max(prev.in, u.in), out: max(prev.out, u.out)}
		p.messages[m.ID] = cur
		p.usage(tokens{in: cur.in - prev.in, out: cur.out - prev.out}, nil)
	}
	for _, b := range blocks {
		switch b.Type {
		case blockText:
			if top && strings.TrimSpace(b.Text) != "" {
				p.lastText = b.Text
				p.progress(harness.KindMessage, clirun.FirstLine(b.Text), now)
			}
		case blockToolUse:
			if b.ID != "" && p.tools[b.ID] {
				continue
			}
			p.tools[b.ID] = true
			p.toolUse(b, now)
		}
	}
}

func (p *parser) toolUse(b block, now time.Time) {
	var in toolInput
	_ = json.Unmarshal(b.Input, &in)
	switch b.Name {
	case toolTodoWrite:
		done := 0
		for _, t := range in.Todos {
			if t.Status == todoCompleted {
				done++
			}
		}
		p.progress(harness.KindMessage, fmt.Sprintf("plan: %d/%d done", done, len(in.Todos)), now)
		return
	case toolStructuredOutput:
		if hasItems(b.Input) {
			p.structured = bytes.Clone(b.Input)
		}
	}
	p.progress(harness.KindTool, toolSummary(b.Name, in), now)
}

func (p *parser) result(ev event, raw []byte, now time.Time) {
	var d tokens
	if ev.Usage != nil {
		tot := ev.Usage.tokens()
		d = tokens{in: max(0, tot.in-p.streamed.in), out: max(0, tot.out-p.streamed.out)}
	}
	var cost *float64
	if p.apiKey && ev.TotalCostUSD != nil {
		c := *ev.TotalCostUSD
		cost = &c
	}
	p.usage(d, cost)
	if hasItems(ev.StructuredOutput) {
		p.structured = bytes.Clone(ev.StructuredOutput)
	}
	if strings.TrimSpace(ev.Result) != "" {
		p.resultText = ev.Result
	}
	if n := len(ev.PermissionDenials); n > 0 {
		names := make([]string, 0, n)
		for _, d := range ev.PermissionDenials {
			names = append(names, d.ToolName)
		}
		p.progress(harness.KindError, "tool calls denied: "+strings.Join(names, ", "), now)
	}
	switch {
	case ev.Subtype == resultMaxTurns:
		p.limit = &harness.LimitError{Limit: harness.LimitTurns, Detail: fmt.Sprintf("claude stopped after %d turns", ev.NumTurns)}
		p.progress(harness.KindError, p.limit.Error(), now)
	case ev.Subtype == resultMaxBudget:
		p.limit = &harness.LimitError{Limit: harness.LimitCost, Detail: "claude stopped at its --max-budget-usd"}
		p.progress(harness.KindError, p.limit.Error(), now)
	case ev.Subtype == resultStructuredFailed:
		// Not fatal (exec returns the Raw despite the exit status): the pipeline checks
		// $FACTORY_RESULT or the last attempt (finalResult), and repairs.
		p.structuredFailed = true
		p.progress(harness.KindError, "the structured output never matched the schema", now)
	case ev.Subtype != resultSuccess || ev.IsError:
		p.failed = true
		p.fail(resultError(ev), raw, now, false)
	}
}

func (p *parser) rateLimitEvent(ev event, raw []byte, now time.Time) {
	info := ev.RateLimitInfo
	if info == nil {
		return
	}
	switch info.Status {
	case rateLimitWarning:
		p.progress(harness.KindMessage, "nearing the usage limit ("+info.RateLimitType+")", now)
	case rateLimitRejected:
		if p.limited {
			return
		}
		until := time.Unix(info.ResetsAt, 0)
		if info.ResetsAt <= 0 || !clirun.ResetPlausible(until, now) {
			until = resetTime(raw, "", now, p.reset)
		}
		p.lastError = "usage limit reached (" + info.RateLimitType + ")"
		p.progress(harness.KindError, p.lastError, now)
		p.limited, p.until = true, until
		p.sink.RateLimited(until)
	}
}

// fail reports an error and checks it for a rate or plan limit (limit: it is one).
func (p *parser) fail(msg string, raw []byte, now time.Time, limit bool) {
	if msg == "" {
		msg = "claude reported an error"
	}
	p.lastError = msg
	p.progress(harness.KindError, clirun.FirstLine(msg), now)
	if p.limited || (!limit && !isRateLimit(msg)) {
		return
	}
	p.limited = true
	p.until = resetTime(raw, msg, now, p.reset)
	p.sink.RateLimited(p.until)
}

func (p *parser) usage(d tokens, cost *float64) {
	if d.in == 0 && d.out == 0 && cost == nil {
		return
	}
	p.streamed.in += d.in
	p.streamed.out += d.out
	p.sink.Usage(harness.UsageDelta{InputTokens: d.in, OutputTokens: d.out, CostUSD: cost})
}

func (p *parser) progress(kind, summary string, at time.Time) {
	p.sink.Progress(harness.ProgressEvent{Kind: kind, Summary: clirun.Clip(summary, harness.MaxSummary), At: at})
}

// finalResult is the run's Result candidate: the structured output, else the final text
// without a Markdown code fence around it. A structured output claude refused is the last
// attempt, with its stringified fields decoded (decodeStringFields).
func (p *parser) finalResult() []byte {
	if len(p.structured) > 0 {
		if p.structuredFailed {
			return decodeStringFields(p.structured)
		}
		return bytes.Clone(p.structured)
	}
	text := p.resultText
	if strings.TrimSpace(text) == "" {
		text = p.lastText
	}
	return clirun.Unfence([]byte(text))
}

// decodeStringFields decodes each top-level field of a JSON object whose value is a string
// holding a JSON object or array: models sometimes stringify a nested object in a tool's input
// (a docs-review run sent StructuredOutput `"data": "{}"` five times, each refused with
// "/data: must be object", until claude gave up). Only those strings change, and only when
// they parse; anything else, or a document that isn't an object, is returned as it is, for
// the pipeline to validate and repair. It is used on a refused structured output only: one
// claude accepted already matched the schema.
func decodeStringFields(b []byte) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil || doc == nil {
		return bytes.Clone(b)
	}
	changed := false
	for k, v := range doc {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		t := bytes.TrimSpace([]byte(s))
		if len(t) == 0 || (t[0] != '{' && t[0] != '[') || !json.Valid(t) {
			continue
		}
		doc[k] = json.RawMessage(t)
		changed = true
	}
	if !changed {
		return bytes.Clone(b)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return bytes.Clone(b)
	}
	return out
}

// resultError is an error result's text: its result, else its errors, else its subtype.
func resultError(ev event) string {
	switch {
	case strings.TrimSpace(ev.Result) != "":
		return ev.Result
	case len(ev.Errors) > 0:
		return strings.Join(ev.Errors, "; ")
	}
	return "claude ended with " + ev.Subtype
}

// hasItems reports whether a JSON value is there and not null or empty.
func hasItems(b json.RawMessage) bool {
	switch s := string(bytes.TrimSpace(b)); s {
	case "", "null", "[]", "{}", `""`:
		return false
	}
	return true
}

func toolSummary(name string, in toolInput) string {
	switch name {
	case toolBash:
		return "$ " + clirun.FirstLine(in.Command)
	case toolWrite, toolEdit, toolMultiEdit:
		return "edit " + in.FilePath
	case toolNotebookEdit:
		return "edit " + in.NotebookPath
	case toolRead:
		return "read " + in.FilePath
	case toolGrep, toolGlob:
		return "search " + in.Pattern
	case toolWebSearch:
		return "search " + clirun.FirstLine(in.Query)
	case toolWebFetch:
		return "fetch " + in.URL
	case toolTask:
		return "agent " + clirun.FirstLine(in.Description)
	case toolSkill:
		return "skill " + in.Skill
	case toolStructuredOutput:
		return "result"
	}
	if rest, ok := strings.CutPrefix(name, mcpToolPrefix); ok {
		server, tool, _ := strings.Cut(rest, "__")
		return "mcp " + server + "." + tool
	}
	return name
}

// isRateLimit reports whether an error's text says a rate or plan limit was hit.
func isRateLimit(msg string) bool { return clirun.ContainsFold(msg, rateLimitWords) }
