package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// Files of the scratch CODEX_HOME besides codex's own.
const (
	// homeDir is the scratch CODEX_HOME inside the workspace's scratch HOME (where codex
	// would look without CODEX_HOME too).
	homeDir = ".codex"
	// sessionFile records the run Repair continues.
	sessionFile = "factory-session.json"
	// promptFile keeps the run's prompt for a Repair that can't resume.
	promptFile = "factory-prompt.md"
	// lastMessageFile is where codex writes the final message (--output-last-message).
	lastMessageFile = "factory-last-message.json"
)

// codexHome is the run's scratch CODEX_HOME.
func codexHome(ws harness.Workspace) string { return filepath.Join(ws.Home, homeDir) }

// Prepare implements harness.Adapter: the task must run on the subscription (an API-key run
// has no gateway injection in the PoC) and the adapter must have the account, and the scratch
// CODEX_HOME gets placeholderAuth's auth.json (0600, never over an existing file),
// renderConfig's config.toml, AGENTS.md (the headless preamble), skills/<name>/ for every
// skill the pipeline materialised in its plugin layout and skills/<plugin>/bin linked to each
// plugin's bin/ (clirun.FlattenSkills).
func (a *Adapter) Prepare(_ context.Context, ws harness.Workspace, t v1alpha1.Task) error {
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		switch h.Resolved.Auth {
		case "", AuthSubscription:
		case AuthAPIKey:
			return errors.New("codex: an API-key run isn't supported in a VM workspace: the gateway injects only the ChatGPT login")
		default:
			return fmt.Errorf("codex: unknown auth %q", h.Resolved.Auth)
		}
	}
	acct := a.o.Account
	if acct == nil || acct.AccountID == "" || acct.UserID == "" || acct.PlanType == "" {
		return errors.New("codex: a VM workspace run needs the ChatGPT account's identifiers (the run bundle's codex/account.json)")
	}
	if ws.Home == "" {
		return errors.New("codex: the workspace has no scratch HOME")
	}
	home := codexHome(ws)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("codex home: %w", err)
	}
	auth, err := placeholderAuth(*acct, a.o.Clock.Now())
	if err != nil {
		return err
	}
	if err := clirun.WriteNew(filepath.Join(home, authFile), auth); err != nil {
		return fmt.Errorf("codex home: the placeholder login: %w", err)
	}
	return writeHome(home, renderConfig(ws), agentsMD(ws), ws)
}

// writeHome writes the scratch CODEX_HOME's config.toml, AGENTS.md and skills/: ws's skill
// folders and its plugins' bin/ links.
func writeHome(home, config, agents string, ws harness.Workspace) error {
	if err := os.WriteFile(filepath.Join(home, configFile), []byte(config), 0o600); err != nil {
		return fmt.Errorf("codex home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, agentsFile), []byte(agents), 0o600); err != nil {
		return fmt.Errorf("codex home: %w", err)
	}
	if err := clirun.FlattenSkills(ws, filepath.Join(home, skillsDir)); err != nil {
		return fmt.Errorf("codex skills: %w", err)
	}
	return nil
}

// placeholderAuth is auth.json, in exactly the shape Phase 0's run used
// (Q5; the keys and fixed values are in flags.go): OPENAI_API_KEY null; tokens.id_token an
// unsigned JWT whose claims are Phase 0's email and far-future exp and the auth claim with
// the account's identifiers, which codex picks its workspace from (it doesn't verify the
// signature); tokens.access_token and tokens.refresh_token harness.Placeholder, which the
// gateway replaces on the way to chatgpt.com; tokens.account_id the account (codex sends it
// as chatgpt-account-id, which the gateway replaces too); and last_refresh now, so codex
// never refreshes the placeholder: the gateway refreshes the real token (P1 §3).
func placeholderAuth(acct runbundle.CodexAccount, now time.Time) ([]byte, error) {
	seg := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("codex: the placeholder login: %w", err)
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	header, err := seg(map[string]string{jwtAlg: jwtAlgNone, jwtTyp: jwtTypJWT})
	if err != nil {
		return nil, err
	}
	claims, err := seg(map[string]any{
		claimEmail:  idTokenEmail,
		claimExpiry: idTokenExpiry,
		claimAuth: map[string]string{
			claimAccountID: acct.AccountID, claimUserID: acct.UserID, claimPlanType: acct.PlanType,
		},
	})
	if err != nil {
		return nil, err
	}
	doc := map[string]any{
		authKeyAPIKey: nil,
		authKeyTokens: map[string]string{
			tokenID:        header + "." + claims + "." + idTokenSignature,
			tokenAccess:    harness.Placeholder,
			tokenRefresh:   harness.Placeholder,
			tokenAccountID: acct.AccountID,
		},
		authKeyLastRefresh: now.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("codex: the placeholder login: %w", err)
	}
	return append(b, '\n'), nil
}

// renderConfig is the scratch config.toml: no update check, and configTail's tables. There is
// no [sandbox_workspace_write]: codex runs danger-full-access in the VM. The model, effort,
// sandbox mode and approval policy are on the command line.
func renderConfig(ws harness.Workspace) string { return configHead + configTail(ws) }

// configHead is the top of every config.toml: the banner and no update check.
const configHead = "# Generated by factory-agent for this run; removed with the workspace.\n" + keyUpdateCheck + " = false\n"

// configTail is config.toml's [shell_environment_policy] and [mcp_servers.factory] tables:
// commands get the whole run environment, and the Effector's MCP server gets the run's
// variables passed through by name (their values never reach the file) and its tools
// approved (keyMCPToolsApproval).
func configTail(ws harness.Workspace) string {
	var b strings.Builder
	line := func(k, v string) { b.WriteString(k + " = " + v + "\n") }
	b.WriteString("\n[" + tableShellEnv + "]\n")
	line(keyInherit, tomlString("all"))
	line(keyIgnoreExcludes, "true")
	b.WriteString("\n[" + tableMCPServers + "." + clirun.EffectorServer + "]\n")
	line(keyMCPCommand, tomlString(clirun.FactoryCLI(ws)))
	line(keyMCPArgs, tomlList(clirun.EffectorArgs()))
	line(keyMCPEnvVars, tomlList(clirun.EnvNames(ws.Env)))
	line(keyMCPToolsApproval, tomlString(mcpToolsApprove))
	return b.String()
}

// agentsMD is the scratch AGENTS.md, where codex runs from the pipeline's WorkDir (workDir):
// the headless preamble and, for a read-only checkout, that the task may not change it, since
// nothing in the VM stops a write to it (the gateway refuses the push).
func agentsMD(ws harness.Workspace) string {
	s := agentsHead(ws)
	if ws.Repo != "" && !ws.RepoWritable {
		s += "\nThe primary checkout, " + ws.Repo + ", is read-only to you: don't change it (a push to it is refused).\n"
	}
	return s
}

// agentsHead is the top of every AGENTS.md: the banner and the headless preamble.
func agentsHead(ws harness.Workspace) string {
	return "<!-- Generated by factory-agent for this run. -->\n\n" + strings.TrimSpace(ws.Preamble) + "\n"
}
