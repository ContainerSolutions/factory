package effector

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/compliance-framework/factory/internal/version"
)

// MCPInstructions are the MCP server's instructions to the harness.
const MCPInstructions = "The factory tools make every change on GitHub for this task (comments, labels, issues, pull requests, " +
	"reviews, review threads, pushes) and validate the task's Result. They are the same ops as the `factory` CLI. " +
	"Only the task's allowed effects succeed; a refusal says why. Repeating a call that failed is safe: it replays."

// Tool is the op's MCP tool: its name, description and JSON Schema input (one property per
// param; the CLI's --<flag>-file variants have no MCP property, the value is given inline).
func (o *Op) Tool() *mcp.Tool {
	props := map[string]any{}
	required := []any{}
	for _, p := range o.Params {
		props[p.Name] = p.jsonSchema()
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return &mcp.Tool{Name: o.Name(), Description: o.Help + " (CLI: " + o.Command() + ")", InputSchema: schema}
}

// NewMCPServer is the Effector's MCP server: one tool per op, each calling e.
func NewMCPServer(e *Effector) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "factory", Title: "CCF Factory Effector", Version: version.Version},
		&mcp.ServerOptions{Instructions: MCPInstructions})
	for _, o := range ops {
		s.AddTool(o.Tool(), e.toolHandler(o))
	}
	return s
}

// ServeMCP serves the MCP protocol on t (stdio in a run) until the client disconnects or
// ctx is done.
func ServeMCP(ctx context.Context, e *Effector, t mcp.Transport) error {
	return NewMCPServer(e).Run(ctx, t)
}

func (e *Effector) toolHandler(o *Op) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := map[string]any{}
		if b := req.Params.Arguments; len(bytes.TrimSpace(b)) > 0 && !bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			if err := dec.Decode(&raw); err != nil {
				return toolError(usagef("%s: the arguments must be a JSON object: %v", o.Name(), err)), nil
			}
		}
		for k := range raw {
			if strings.ContainsRune(k, 0) { // the CLI's file keys are not MCP arguments
				return toolError(usagef("%s: unknown argument %q", o.Name(), k)), nil
			}
		}
		out, err := e.do(ctx, o, raw, nil)
		if err != nil {
			return toolError(err), nil
		}
		text, err := json.Marshal(out)
		if err != nil {
			return toolError(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}, StructuredContent: out}, nil
	}
}

// toolError is a failed op as a tool result the model sees (isError), not a protocol error.
func toolError(err error) *mcp.CallToolResult {
	body := map[string]any{"error": errorJSON(err)}
	text, _ := json.Marshal(body)
	return &mcp.CallToolResult{IsError: true, StructuredContent: body,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error() + "\n" + string(text)}}}
}
