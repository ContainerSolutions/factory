package clirun

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
)

// EffectorServer is the Effector's MCP server name in every harness's MCP config. Its command
// is the workspace's `factory` CLI (`factory-agent fx`) with EffectorArgs, i.e.
// `factory-agent fx mcp`, the same server as `factory-agent mcp` (effector.md).
const EffectorServer = "factory"

// EffectorArgs are the arguments of the Effector's MCP server.
func EffectorArgs() []string { return []string{"mcp"} }

// FactoryCLI is the workspace's `factory` CLI: the Effector's MCP server and the hooks run it.
func FactoryCLI(ws harness.Workspace) string { return filepath.Join(ws.Bin, "factory") }

// EnvNames are the variable names in env, sorted and unique: what a harness passes through
// to the Effector by name, so no value (a token among them) is written to a config file.
func EnvNames(env []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
