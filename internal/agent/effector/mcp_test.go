package effector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ContainerSolutions/factory/internal/agent/effector"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
)

// connect serves e over an in-memory transport and returns a connected client session.
func connect(t *testing.T, e *effector.Effector) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	srv := effector.NewMCPServer(e)
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- ss.Wait() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

// cliFlags are the flags `factory <op>` defines, from its -h output.
func cliFlags(t *testing.T, o *effector.Op) []string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := effector.Main(context.Background(), effector.New(effector.Options{}), append(slices.Clone(o.Words), "-h"), nil, &out, &errb); code != 0 {
		t.Fatalf("%s -h = %d", o.Command(), code)
	}
	var flags []string
	for _, line := range strings.Split(errb.String(), "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(line), "-"); ok {
			name, _, _ := strings.Cut(f, " ")
			flags = append(flags, name)
		}
	}
	sort.Strings(flags)
	return flags
}

// TestMCPToolsMatchTheCLI: tools/list is the CLI's op table exactly: one tool per op, in
// order, each property one CLI flag (the CLI adds only the --<flag>-file variants), the
// same required params.
func TestMCPToolsMatchTheCLI(t *testing.T) {
	t.Parallel()
	cs := connect(t, effector.New(effector.Options{}))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ops := effector.Ops()
	var names, want []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, o := range ops {
		want = append(want, o.Name())
	}
	sort.Strings(names)
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools %q, want the ops %q", names, want)
	}
	if len(ops) != 19 {
		t.Errorf("%d ops, want the 18 of effector.md's table and link-duplicate", len(ops))
	}
	for _, tool := range res.Tools {
		o := ops[slices.IndexFunc(ops, func(o *effector.Op) bool { return o.Name() == tool.Name })]
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
			Additional *bool                     `json:"additionalProperties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Type != "object" || schema.Additional == nil || *schema.Additional {
			t.Errorf("%s: schema %s", tool.Name, b)
		}
		var props, flags, required, wantRequired []string
		for p := range schema.Properties {
			props = append(props, p)
		}
		for _, p := range o.Params {
			flags = append(flags, p.CLIFlag())
			if p.Kind == effector.Text || p.Kind == effector.JSON {
				flags = append(flags, p.CLIFlag()+"-file")
			}
			if p.Required {
				wantRequired = append(wantRequired, p.Name)
			}
			if !slices.Contains(props, p.Name) {
				t.Errorf("%s: no property for param %s", tool.Name, p.Name)
			}
		}
		required = schema.Required
		sort.Strings(flags)
		if got := cliFlags(t, o); !slices.Equal(got, flags) {
			t.Errorf("%s: CLI flags %q, want %q", o.Command(), got, flags)
		}
		if len(props) != len(o.Params) {
			t.Errorf("%s: properties %q for %d params", tool.Name, props, len(o.Params))
		}
		if !slices.Equal(required, wantRequired) {
			t.Errorf("%s: required %q, want %q", tool.Name, required, wantRequired)
		}
		if !strings.Contains(tool.Description, o.Command()) {
			t.Errorf("%s: description %q doesn't name the CLI", tool.Name, tool.Description)
		}
	}
}

// TestMCPCallRoundTrips: tools/call goes through the same Do as the CLI: the same request
// for the same arguments, the same refusals.
func TestMCPCallRoundTrips(t *testing.T) {
	t.Parallel()
	d := newDispatcher(t)
	r := newRun(t, d, func(c *pipeline.EffectorConfig) { c.Allow = []string{"label.add", "pr.open", "comment"} })
	cs := connect(t, r.effector())
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "pr_open", Arguments: map[string]any{
		"head": "factory/tsk_1", "base": "main", "title": "docs: the intro", "body": "Closes #43.", "draft": true, "labels": []any{"docs"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("pr_open: %v", res.Content)
	}
	out, _ := res.StructuredContent.(map[string]any)
	if out["ref"] != "acme/docs#9" || out["effectId"] == "" {
		t.Errorf("structured %v", res.StructuredContent)
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &text); err != nil || !reflect.DeepEqual(text, out) {
		t.Errorf("text %v %v", res.Content, err)
	}
	// The CLI sends the same request (the next call with these arguments, so .1).
	if code, _, stderr := r.cli("pr", "open", "--head", "factory/tsk_1", "--base", "main", "--title", "docs: the intro", "--body", "Closes #43.", "--draft", "--labels", "docs"); code != 0 {
		t.Fatal(stderr)
	}
	reqs := d.requests()
	mcpReq, cliReq := reqs[0].Body, reqs[1].Body
	if mk, ck := mcpReq["idempotencyKey"].(string), cliReq["idempotencyKey"].(string); strings.TrimSuffix(mk, ".0") != strings.TrimSuffix(ck, ".1") {
		t.Errorf("keys %q %q", mk, ck)
	}
	delete(mcpReq, "idempotencyKey")
	delete(cliReq, "idempotencyKey")
	if !reflect.DeepEqual(mcpReq, cliReq) {
		t.Errorf("MCP %v != CLI %v", mcpReq, cliReq)
	}
	// A number is a fine PR reference; refusals are tool errors with the exit code.
	for _, tt := range []struct {
		name string
		args map[string]any
		code float64
		msg  string
	}{
		{"pr_merge", map[string]any{"pr": 9}, 3, "pr.merge is not allowed"},
		{"label_add", map[string]any{"labels": []any{"wontfix"}}, 3, `label "wontfix" (labels) is not allowed`},
		{"comment", map[string]any{}, 2, "body (--body or --body-file) is required"},
		{"comment", map[string]any{"body": "x", "bodyFile": "/etc/passwd"}, 2, "unknown argument bodyFile"},
		{"comment", map[string]any{"body": 3.5}, 2, "not a string"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tt.name, Arguments: tt.args})
		if err != nil {
			t.Fatal(err)
		}
		e, _ := res.StructuredContent.(map[string]any)["error"].(map[string]any)
		if !res.IsError || e["exitCode"] != tt.code || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, tt.msg) {
			t.Errorf("%s %v: %v %v", tt.name, tt.args, res.IsError, res.Content)
		}
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rm_rf", Arguments: map[string]any{}}); err == nil {
		t.Error("an unknown tool isn't a protocol error")
	}
	if n := len(d.requests()); n != 2 {
		t.Errorf("%d requests: the refusals reached the dispatcher", n)
	}
}

// TestMCPStdio: the server speaks newline-delimited JSON-RPC over plain pipes.
func TestMCPStdio(t *testing.T) {
	t.Parallel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- effector.ServeMCP(context.Background(), effector.New(effector.Options{}), &mcp.IOTransport{Reader: inR, Writer: outW})
		_ = outW.Close()
	}()
	dec := json.NewDecoder(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() map[string]any {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if init := recv(); init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "factory" {
		t.Errorf("initialize %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := recv()
	if tools := list["result"].(map[string]any)["tools"].([]any); len(tools) != len(effector.Ops()) {
		t.Errorf("%d tools", len(tools))
	}
	_ = inW.Close()
	if err := <-done; err != nil && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "closed") {
		t.Errorf("ServeMCP: %v", err)
	}
}
