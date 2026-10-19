package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Bounds besides the stream's (clirun bounds that).
const (
	maxSchema = 1 << 20 // the Result schema passed inline
	maxDetail = 300     // error text in an error
)

// session is what Repair needs from the Run: the session to resume and the settings to run
// it with. It lives in the scratch directory, so the Adapter holds no per-run state.
type session struct {
	SessionID    string  `json:"sessionId,omitempty"`
	Model        string  `json:"model,omitempty"`
	Auth         string  `json:"auth"`
	WorkDir      string  `json:"workDir"`
	ReadOnly     bool    `json:"readOnly"`
	MaxBudgetUSD float64 `json:"maxBudgetUSD,omitempty"`
}

// Run implements harness.Adapter: `claude -p` on the prompt (stdin) with the task's resolved
// model and auth, in the workspace's working directory, the write tools disallowed unless the
// primary checkout is writable.
func (a *Adapter) Run(ctx context.Context, ws harness.Workspace, t v1alpha1.Task, prompt string, sink harness.EventSink) (harness.Raw, error) {
	s := session{Model: a.o.Config.Default, WorkDir: ws.WorkDir, ReadOnly: ws.Repo == "" || !ws.RepoWritable}
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		if h.Resolved.Model != "" {
			s.Model = h.Resolved.Model
		}
		s.Auth = h.Resolved.Auth
	}
	if s.WorkDir == "" {
		s.WorkDir = ws.Root
	}
	if s.Auth == "" {
		auth, err := a.authMode(a.o.Config.ConfigDir)
		if err != nil {
			return harness.Raw{}, err
		}
		s.Auth = auth
	}
	maxTurns := 0
	if l := t.Spec.Limits; l != nil {
		maxTurns = l.MaxTurns
		if s.Auth == AuthAPIKey {
			s.MaxBudgetUSD = l.MaxCostUSD // a subscription run has no cost
		}
	}
	dir := scratch(ws)
	if err := os.WriteFile(filepath.Join(dir, promptFile), []byte(prompt), 0o600); err != nil {
		return harness.Raw{}, fmt.Errorf("claude-code: keep the prompt: %w", err)
	}
	raw, sessionID, err := a.exec(ctx, ws, s, prompt, maxTurns, sink)
	s.SessionID = sessionID
	if werr := writeSession(dir, s); werr != nil && err == nil {
		err = werr
	}
	return raw, err
}

// Repair implements harness.Adapter: the same session resumed (`claude -p --resume <id>`)
// with the problems, asking for a valid Result only, in at most repairMaxTurns turns. Without
// a session to resume, a new run gets the original prompt followed by the problems.
func (a *Adapter) Repair(ctx context.Context, ws harness.Workspace, problems []string, sink harness.EventSink) (harness.Raw, error) {
	dir := scratch(ws)
	s, err := readSession(dir)
	if err != nil {
		return harness.Raw{}, err
	}
	prompt := clirun.RepairPrompt(problems, ws)
	if s.SessionID == "" {
		orig, err := os.ReadFile(filepath.Join(dir, promptFile))
		if err != nil {
			return harness.Raw{}, fmt.Errorf("claude-code: no session to resume and no prompt: %w", err)
		}
		prompt = string(orig) + "\n\n---\n\n" + prompt
	}
	raw, sessionID, err := a.exec(ctx, ws, s, prompt, repairMaxTurns, sink)
	if sessionID != "" && sessionID != s.SessionID {
		s.SessionID = sessionID
		_ = writeSession(dir, s)
	}
	return raw, err
}

// exec runs one claude process to its end and returns its raw output and session id.
func (a *Adapter) exec(ctx context.Context, ws harness.Workspace, s session, prompt string, maxTurns int, sink harness.EventSink) (harness.Raw, string, error) {
	env, err := a.runEnv(ws, s)
	if err != nil {
		return harness.Raw{}, s.SessionID, err
	}
	schema, err := compactSchema(ws.Schema)
	if err != nil {
		return harness.Raw{}, s.SessionID, err
	}
	dir := scratch(ws)
	args := runArgs(runOptions{
		Model: s.Model, Preamble: strings.TrimSpace(ws.Preamble), PluginDirs: pluginDirs(ws),
		Settings: filepath.Join(dir, settingsFile), MCPConfig: filepath.Join(dir, mcpFile), Schema: schema,
		MaxTurns: maxTurns, MaxBudgetUSD: s.MaxBudgetUSD, ReadOnly: s.ReadOnly, AddDirs: addDirs(ws, s.WorkDir),
		Resume: s.SessionID,
	})

	tr := &clirun.Transcript{Name: "claude"}
	tr.Factory("claude-exec", map[string]any{"args": args})
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	p := newParser(sink, a.o.Clock, a.o.Throttle, s.Auth == AuthAPIKey)
	p.onFatal = stop
	exit, err := clirun.Run(runCtx, clirun.Process{
		Binary: a.o.Binary, Args: args, Dir: s.WorkDir, Env: env,
		Stdin: strings.NewReader(prompt), KillGrace: a.o.KillGrace,
		Line: func(line []byte) { tr.Add(line); p.line(line) },
	})
	if err != nil {
		return harness.Raw{Transcript: tr.Bytes()}, s.SessionID, fmt.Errorf("claude: %w", err)
	}
	if strings.TrimSpace(exit.Stderr) != "" {
		tr.Factory("claude-stderr", map[string]any{"text": exit.Stderr})
	}
	raw := harness.Raw{Transcript: tr.Bytes(), Result: p.finalResult()}
	sessionID := p.sessionID
	if sessionID == "" {
		sessionID = s.SessionID
	}

	switch {
	case ctx.Err() != nil:
		return raw, sessionID, ctx.Err()
	case p.fatal != "":
		return raw, sessionID, fmt.Errorf("claude: %s", p.fatal)
	case p.limited:
		return raw, sessionID, fmt.Errorf("claude: rate limited until %s: %s", p.until.UTC().Format(time.RFC3339), clirun.Clip(p.lastError, maxDetail))
	case p.limit != nil:
		return raw, sessionID, p.limit
	case exit.Err != nil:
		tail := clirun.LastLine(exit.Stderr)
		if isRateLimit(tail) {
			until := resetTime(nil, tail, a.o.Clock.Now(), a.o.Throttle)
			sink.RateLimited(until)
			return raw, sessionID, fmt.Errorf("claude: rate limited until %s: %s", until.UTC().Format(time.RFC3339), clirun.Clip(tail, maxDetail))
		}
		detail := p.lastError
		if detail == "" {
			detail = tail
		}
		return raw, sessionID, fmt.Errorf("claude: %w: %s", exit.Err, clirun.Clip(detail, maxDetail))
	case p.failed:
		return raw, sessionID, fmt.Errorf("claude: the run failed: %s", clirun.Clip(p.lastError, maxDetail))
	}
	return raw, sessionID, nil
}

// runEnv is claude's environment: the pipeline's (already reduced to its allow-list), the
// dedicated CLAUDE_CONFIG_DIR, runEnv's settings, and the one credential variable the auth
// mode needs from the worker's environment: ANTHROPIC_API_KEY for an API-key run, or a
// subscription's CLAUDE_CODE_OAUTH_TOKEN when the owner set one. USER is passed too: claude
// looks its macOS Keychain entry up by it.
func (a *Adapter) runEnv(ws harness.Workspace, s session) ([]string, error) {
	env := clirun.WithEnv(ws.Env, envConfigDir, a.o.Config.ConfigDir)
	for _, kv := range runEnv {
		env = clirun.WithEnv(env, kv[0], kv[1])
	}
	owner := a.o.Environ()
	if u := clirun.LookupEnv(owner, "USER"); u != "" {
		env = clirun.WithEnv(env, "USER", u)
	}
	switch s.Auth {
	case AuthAPIKey:
		key := clirun.LookupEnv(owner, envAPIKey)
		if key == "" {
			return nil, fmt.Errorf("claude-code: the task runs on an API key, but %s isn't set in the worker's environment", envAPIKey)
		}
		env = clirun.WithEnv(env, envAPIKey, key)
	case AuthSubscription:
		if tok := clirun.LookupEnv(owner, envOAuthToken); tok != "" {
			env = clirun.WithEnv(env, envOAuthToken, tok)
		}
	default:
		return nil, fmt.Errorf("claude-code: unknown auth %q", s.Auth)
	}
	return env, nil
}

// addDirs are the directories the tools get besides the working directory: out/ and the
// other checkouts.
func addDirs(ws harness.Workspace, workDir string) []string {
	var out []string
	for _, d := range append([]string{ws.Out}, mountDirs(ws)...) {
		if d != "" && d != workDir {
			out = append(out, d)
		}
	}
	return out
}

func mountDirs(ws harness.Workspace) []string {
	out := make([]string, 0, len(ws.Mounts))
	for _, m := range ws.Mounts {
		out = append(out, m.Dir)
	}
	return out
}

// compactSchema is the Result schema file as one line of JSON ("" when there is none).
func compactSchema(p string) (string, error) {
	b, err := clirun.ReadLimited(p, maxSchema+1)
	if os.IsNotExist(err) || (err == nil && len(bytes.TrimSpace(b)) == 0) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("claude-code: the Result schema: %w", err)
	}
	if len(b) > maxSchema {
		return "", fmt.Errorf("claude-code: the Result schema is over %d bytes", maxSchema)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return "", fmt.Errorf("claude-code: the Result schema: %w", err)
	}
	return out.String(), nil
}

func writeSession(dir string, s session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), b, 0o600); err != nil {
		return fmt.Errorf("claude-code: record the session: %w", err)
	}
	return nil
}

func readSession(dir string) (session, error) {
	var s session
	b, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		return s, fmt.Errorf("claude-code: no run to repair: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("claude-code: the session record: %w", err)
	}
	return s, nil
}
