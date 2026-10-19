package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/compliance-framework/factory/internal/agent/effector"
)

// mcpCommand is `factory-agent mcp`, the Effector's stdio MCP server (effector.md §Shape).
// The harnesses' MCP configs run it as `bin/factory mcp`, i.e. `factory-agent fx mcp`:
// the same server.
func mcpCommand() command {
	return command{
		name:    "mcp",
		summary: "the Effector's stdio MCP server (the `factory` ops as tools)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runMCP(ctx, args, os.Stdin, stdout, stderr, os.Getenv)
		},
	}
}

// runMCP serves MCP on stdin/stdout until the client closes stdin. Exit 2 is a usage error,
// 4 a server failure.
func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: factory-agent mcp   (or factory-agent fx mcp; it serves MCP on stdin/stdout)")
		return 2
	}
	e, err := effector.FromEnv(getenv, nil)
	if err != nil {
		fmt.Fprintln(stderr, "factory mcp:", err)
		return 2
	}
	t := &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}
	if err := effector.ServeMCP(ctx, e, t); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "factory mcp:", err)
		return 4
	}
	return 0
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
