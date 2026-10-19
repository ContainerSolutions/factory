package claudecode

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
)

// sandboxSupport is nil when claude's OS sandbox can run on this machine: the platform has
// one and its tools are on PATH. The adapter never runs claude without it.
func (a *Adapter) sandboxSupport() error {
	tools, ok := sandboxTools[a.o.GOOS]
	if !ok {
		return fmt.Errorf("claude-code: Claude Code's OS sandbox isn't supported on %s, and the agent never runs claude without it", a.o.GOOS)
	}
	var missing []string
	for _, t := range tools {
		if _, err := a.o.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("claude-code: Claude Code's OS sandbox needs %s on PATH (on Linux: install bubblewrap and socat), and the agent never runs claude without it",
			strings.Join(missing, " and "))
	}
	return nil
}

// sandboxWritable are where sandboxed commands may write: out/, tmp/, the scratch HOME and
// the checkouts the harness may write. A read-only workspace has no writable checkout.
func sandboxWritable(ws harness.Workspace) []string {
	return appendUnique(writableRoots(ws), ws.Home)
}

// sandboxDomains are the hosts sandboxed commands may reach: the run's (the git host, with
// github.com's API and raw content, the dispatcher, the local stack and the Task's hosts),
// and the harness's own.
func sandboxDomains(ws harness.Workspace) []string {
	var out []string
	for _, h := range ws.Hosts {
		out = appendUnique(out, h)
		for _, d := range hostDomains[h] {
			out = appendUnique(out, d)
		}
	}
	for _, d := range harnessDomains {
		out = appendUnique(out, d)
	}
	return out
}

// sandboxDeniedReads are the paths no tool or sandboxed command may read: deniedHomePaths
// under the worker's HOME (ownerHome) and the dedicated config dir, except one that holds
// the workspace.
func sandboxDeniedReads(ws harness.Workspace, ownerHome, configDir string) []string {
	var paths []string
	if filepath.IsAbs(ownerHome) {
		for _, p := range deniedHomePaths {
			paths = append(paths, filepath.Join(ownerHome, filepath.FromSlash(p)))
		}
	}
	if filepath.IsAbs(configDir) {
		paths = append(paths, filepath.Clean(configDir))
	}
	var out []string
	for _, p := range paths {
		if p == ws.Root || strings.HasPrefix(ws.Root, p+string(filepath.Separator)) {
			continue
		}
		out = appendUnique(out, p)
	}
	return out
}

// sandboxSettings are the settings file's sandbox block and permission rules: the sandbox on
// with no escape and no exempt command; writes only in sandboxWritable, never in the
// adapter's scratch dir (its settings and hook); no read of sandboxDeniedReads; the network
// only to sandboxDomains, plus the git credential helper's socket.
func sandboxSettings(ws harness.Workspace, ownerHome, configDir string) (sandbox, permissions map[string]any) {
	var sockets []string
	if s := clirun.LookupEnv(ws.Env, gitcred.EnvSocket); s != "" {
		sockets = append(sockets, s)
	}
	sandbox = map[string]any{
		keySandboxEnabled:   true,
		keyAllowUnsandboxed: false,
		keyAutoAllowBash:    true,
		keyExcludedCommands: []string{},
		keyWeakerNested:     false,
		keySandboxNetwork: map[string]any{
			keyAllowUnixSockets:  nonNil(sockets),
			keyAllowLocalBinding: false,
		},
	}
	var allow, deny []string
	for _, d := range sandboxWritable(ws) {
		allow = append(allow, fmt.Sprintf(ruleEditFormat, d))
	}
	for _, d := range sandboxDomains(ws) {
		allow = append(allow, fmt.Sprintf(ruleDomainFormat, d))
	}
	deny = append(deny, fmt.Sprintf(ruleEditFormat, scratch(ws)))
	for _, p := range sandboxDeniedReads(ws, ownerHome, configDir) {
		deny = append(deny, fmt.Sprintf(ruleReadFormat, p), fmt.Sprintf(ruleReadAllForm, p))
	}
	permissions = map[string]any{keyAllow: allow, keyDeny: deny}
	return sandbox, permissions
}

func appendUnique(list []string, s string) []string {
	if s == "" || slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
