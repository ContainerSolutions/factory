package opencode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// The gateway's own answers on the model endpoint, which opencode quotes in an error: no-login
// (the agent host has no model server: the run fails) and upstream or timeout (the server or
// the tunnel is down: the task is throttled for OutageThrottle).
var (
	gatewayNoLogin = []string{refusalText(policy.CodeNoLogin)}
	gatewayOutage  = []string{refusalText(policy.CodeUpstream), refusalText(policy.CodeTimeout)}
)

func refusalText(c policy.Code) string {
	r, _ := c.Response("")
	return strings.ToLower(strings.TrimSpace(r.Body))
}

// parser turns `opencode run --format json` lines into the EventSink's progress, usage and
// rate limits (harness-adapters.md §OpenCode, Stream mapping). It is not safe for concurrent
// use: one goroutine reads one process's stdout.
type parser struct {
	sink  harness.EventSink
	clk   clock.Clock
	reset time.Duration // the throttle when a rate limit gives no reset

	sessionID string
	turns     int
	lastText  string
	lastError string
	recovered bool // a step finished after the last error: opencode carried on (a compacted overflow)
	limited   bool
	until     time.Time
}

func newParser(sink harness.EventSink, clk clock.Clock, reset time.Duration) *parser {
	return &parser{sink: sink, clk: clk, reset: reset}
}

// event is one JSON line: the fields of every event type the parser reads.
type event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Error     json.RawMessage `json:"error"`
	Part      *struct {
		Text   string  `json:"text"`
		Tool   string  `json:"tool"`
		Tokens *tokens `json:"tokens"`
		State  struct {
			Input toolInput `json:"input"`
		} `json:"state"`
	} `json:"part"`
}

type toolInput struct {
	Command  string `json:"command"`
	FilePath string `json:"filePath"`
	Pattern  string `json:"pattern"`
	Name     string `json:"name"`
}

// tokens are a step's: input excludes the cached input and output the reasoning
// (packages/opencode/src/session/session.ts, getUsage).
type tokens struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	Reasoning float64 `json:"reasoning"`
	Cache     struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
	} `json:"cache"`
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
	if p.sessionID == "" {
		p.sessionID = ev.SessionID
	}
	now := p.clk.Now()
	switch {
	case ev.Type == evStepStart:
		p.turns++
		p.progress(harness.KindTurn, fmt.Sprintf("turn %d", p.turns), now)
	case ev.Type == evStepFinish && ev.Part != nil && ev.Part.Tokens != nil:
		p.recovered = p.lastError != ""
		t := ev.Part.Tokens
		in, out := int64(t.Input+t.Cache.Read+t.Cache.Write), int64(t.Output+t.Reasoning)
		if in > 0 || out > 0 {
			// A self-hosted model has no price: no cost.
			p.sink.Usage(harness.UsageDelta{InputTokens: in, OutputTokens: out})
		}
	case ev.Type == evToolUse && ev.Part != nil:
		p.progress(harness.KindTool, toolSummary(ev.Part.Tool, ev.Part.State.Input), now)
	case ev.Type == evText && ev.Part != nil && strings.TrimSpace(ev.Part.Text) != "":
		p.lastText = ev.Part.Text
		p.progress(harness.KindMessage, clirun.FirstLine(ev.Part.Text), now)
	case ev.Type == evError:
		msg, detail := errorText(ev.Error)
		p.lastError, p.recovered = msg, false
		p.progress(harness.KindError, clirun.FirstLine(msg), now)
		p.throttle(detail, b)
	}
}

// throttle reports RateLimited, once, when text is the gateway's outage answer (until now +
// OutageThrottle) or a rate limit (until its reset, else now + the parser's reset); the
// gateway's no-login answer is neither. It reports whether the run is now limited.
func (p *parser) throttle(text string, raw []byte) bool {
	if p.limited {
		return true
	}
	now := p.clk.Now()
	switch {
	case clirun.ContainsFold(text, gatewayNoLogin):
		return false
	case clirun.ContainsFold(text, gatewayOutage):
		p.until = now.Add(OutageThrottle)
	case clirun.ContainsFold(text, rateLimitWords):
		p.until = clirun.ResetTime(raw, text, now, p.reset)
	default:
		return false
	}
	p.limited = true
	p.sink.RateLimited(p.until)
	return true
}

func (p *parser) progress(kind, summary string, at time.Time) {
	p.sink.Progress(harness.ProgressEvent{Kind: kind, Summary: clirun.Clip(summary, harness.MaxSummary), At: at})
}

// errorText is an error event's message (data.message, message, name, or the error as a
// string) and the text a throttle is looked for in: the message, the response body and the
// HTTP status.
func errorText(e json.RawMessage) (msg, detail string) {
	var obj struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message      string `json:"message"`
			StatusCode   int    `json:"statusCode"`
			ResponseBody string `json:"responseBody"`
		} `json:"data"`
	}
	if json.Unmarshal(e, &obj) != nil {
		_ = json.Unmarshal(e, &msg)
	}
	msg = cmp.Or(obj.Data.Message, obj.Message, obj.Name, msg, "opencode reported an error")
	detail = msg + "\n" + obj.Data.ResponseBody
	if obj.Data.StatusCode != 0 {
		detail += fmt.Sprintf("\nHTTP %d", obj.Data.StatusCode)
	}
	return msg, detail
}

// toolSummary is a tool event's summary, as claude's are: the tool and its main input, else
// its name.
func toolSummary(name string, in toolInput) string {
	switch name {
	case toolBash:
		return "$ " + clirun.FirstLine(in.Command)
	case toolEdit, toolWrite:
		return "edit " + in.FilePath
	case toolRead:
		return "read " + in.FilePath
	case toolGlob, toolGrep:
		return "search " + in.Pattern
	case toolSkill:
		return "skill " + in.Name
	}
	if tool, ok := strings.CutPrefix(name, clirun.EffectorServer+mcpToolSep); ok {
		return "mcp " + clirun.EffectorServer + "." + tool
	}
	return name
}
