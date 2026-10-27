package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/agent/internal/execfile"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
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
// plugin's bin/ (linkPluginBins).
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
	if err := writeNew(filepath.Join(home, authFile), auth); err != nil {
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
	if err := flattenSkills(ws.SkillList, filepath.Join(home, skillsDir)); err != nil {
		return fmt.Errorf("codex skills: %w", err)
	}
	if err := linkPluginBins(ws, filepath.Join(home, skillsDir)); err != nil {
		return fmt.Errorf("codex skills: %w", err)
	}
	return nil
}

// writeNew writes b to a new file p (0600); an existing file or symlink at p is an error.
func writeNew(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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

// flattenSkills copies each skill folder to dst/<name>/ (codex's layout). Two plugins'
// skills of the same name both go in as <plugin>-<name>.
func flattenSkills(skills []harness.Skill, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	count := map[string]int{}
	for _, s := range skills {
		count[s.Name]++
	}
	used := map[string]bool{}
	for _, s := range skills {
		name := s.Name
		if count[name] > 1 && s.Plugin != "" {
			name = s.Plugin + "-" + s.Name
		}
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || used[name] {
			return fmt.Errorf("skill %q (plugin %q): can't name its folder", s.Name, s.Plugin)
		}
		used[name] = true
		if err := copyTree(s.Dir, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// linkPluginBins links dst/<plugin>/bin to the pipeline's skills/<plugin>/bin for every plugin
// of ws's skills that has a bin/ directory, unless dst/<plugin>/bin is already there (a skill
// folder of that name with its own bin/). A skill finds another plugin's CLI beside its own
// folder: lisa's self-review runs <lisa skill dir>/../ccf-review/bin/ccfr, which is
// skills/ccf-review/bin/ccfr in codex's flat layout. A link rather than a copy: ccfr finds its
// lib/ and rules from its real path, the plugin root that stays in the pipeline's skills/.
func linkPluginBins(ws harness.Workspace, dst string) error {
	if ws.Skills == "" {
		return nil
	}
	var done []string
	for _, s := range ws.SkillList {
		p := s.Plugin
		if p == "" || slices.Contains(done, p) {
			continue
		}
		done = append(done, p)
		if strings.ContainsAny(p, `/\`) || p == "." || p == ".." {
			return fmt.Errorf("plugin %q: can't name its folder", p)
		}
		src := filepath.Join(ws.Skills, p, "bin")
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			continue
		}
		link := filepath.Join(dst, p, "bin")
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dst, p), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(src, link); err != nil {
			return fmt.Errorf("plugin %s: link bin/: %w", p, err)
		}
	}
	return nil
}

// copyTree copies the directories and regular files under src to dst (0700 directories,
// files 0700 when executable, else 0600); symlinks and special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if fi.Mode()&0o111 != 0 {
				mode = 0o700
			}
			return copyFile(p, target, mode)
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return execfile.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode, func(out *os.File) error {
		_, err := io.Copy(out, in)
		return err
	})
}
