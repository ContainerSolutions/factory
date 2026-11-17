package opencode

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// maxDetail bounds the error text in an error.
const maxDetail = 300

// What Run keeps for Repair beside instructions.md, in <home>/.factory-opencode/.
const (
	sessionFile = "factory-session.json"
	promptFile  = "factory-prompt.md"
	// inputFile is one invocation's prompt as --file, when promptOnStdin is off.
	inputFile = "factory-input.md"
)

// session is what Repair needs from the Run: the session to resume, its model and where it
// ran. The permissions and the effort are in opencode.json, which the repair loads again. It
// lives in the workspace, so the Adapter holds no per-run state.
type session struct {
	SessionID string `json:"sessionId,omitempty"`
	Model     string `json:"model"`
	WorkDir   string `json:"workDir"`
}

// Run implements harness.Adapter: `opencode run --format json` on the prompt with the task's
// resolved model (else the config's default), from the pipeline's WorkDir (else the workspace
// root), in the environment runEnv gives.
func (a *Adapter) Run(ctx context.Context, ws harness.Workspace, t v1alpha1.Task, prompt string, sink harness.EventSink) (harness.Raw, error) {
	s := session{Model: a.model(resolved(t)), WorkDir: cmp.Or(ws.WorkDir, ws.Root)}
	if s.Model == "" || s.WorkDir == "" || ws.Home == "" {
		return harness.Raw{}, errors.New("opencode: no model, or the workspace has no directory to run in or no scratch HOME")
	}
	dir := filepath.Join(ws.Home, ownDir)
	if err := os.WriteFile(filepath.Join(dir, promptFile), []byte(prompt), 0o600); err != nil {
		return harness.Raw{}, fmt.Errorf("opencode: keep the prompt: %w", err)
	}
	raw, sessionID, err := a.exec(ctx, ws, s, prompt, sink)
	s.SessionID = sessionID
	if werr := writeSession(dir, s); werr != nil && err == nil {
		err = werr
	}
	return raw, err
}

// Repair implements harness.Adapter: the Run's session resumed (--session) with the problems,
// asking for a valid Result only. Without a session to resume, a new run gets the original
// prompt followed by the problems.
func (a *Adapter) Repair(ctx context.Context, ws harness.Workspace, problems []string, sink harness.EventSink) (harness.Raw, error) {
	dir := filepath.Join(ws.Home, ownDir)
	s, err := readSession(dir)
	if err != nil {
		return harness.Raw{}, err
	}
	prompt := clirun.RepairPrompt(problems, ws)
	if s.SessionID == "" {
		orig, err := os.ReadFile(filepath.Join(dir, promptFile))
		if err != nil {
			return harness.Raw{}, fmt.Errorf("opencode: no session to resume and no prompt: %w", err)
		}
		prompt = string(orig) + "\n\n---\n\n" + prompt
	}
	raw, _, err := a.exec(ctx, ws, s, prompt, sink)
	return raw, err
}

// exec runs one opencode process to its end and returns its raw output and session id.
func (a *Adapter) exec(ctx context.Context, ws harness.Workspace, s session, prompt string, sink harness.EventSink) (harness.Raw, string, error) {
	args := []string{cmdRun, flagFormat, formatJSON, flagModel, providerID + "/" + s.Model}
	if s.SessionID != "" {
		args = append(args, flagSession, s.SessionID)
	}
	var stdin io.Reader = strings.NewReader(prompt)
	if !promptOnStdin {
		in := filepath.Join(ws.Home, ownDir, inputFile)
		if err := os.WriteFile(in, []byte(prompt), 0o600); err != nil {
			return harness.Raw{}, "", fmt.Errorf("opencode: %w", err)
		}
		args, stdin = append(slices.Insert(args, 1, fileMessage), flagFile, in), nil
	}
	tr := &clirun.Transcript{Name: Name}
	tr.Factory("opencode-exec", map[string]any{"args": args})
	p := newParser(sink, a.o.Clock, a.o.Throttle)
	exit, err := clirun.Run(ctx, clirun.Process{
		Binary: a.o.Binary, Args: args, Dir: s.WorkDir, Env: runEnv(ws), Stdin: stdin, KillGrace: a.o.KillGrace,
		Line: func(line []byte) { tr.Add(line); p.line(line) },
	})
	if err != nil {
		return harness.Raw{Transcript: tr.Bytes()}, "", fmt.Errorf("opencode: %w", err)
	}
	if strings.TrimSpace(exit.Stderr) != "" {
		tr.Factory("opencode-stderr", map[string]any{"text": exit.Stderr})
	}
	raw := harness.Raw{Transcript: tr.Bytes(), Result: clirun.Unfence([]byte(p.lastText))}
	if ctx.Err() != nil {
		return raw, p.sessionID, ctx.Err()
	}
	tail := clirun.LastLine(exit.Stderr)
	if exit.Err != nil && !p.limited && p.throttle(tail, nil) {
		p.lastError = tail
	}
	switch {
	case p.limited:
		return raw, p.sessionID, fmt.Errorf("opencode: rate limited until %s: %s", p.until.UTC().Format(time.RFC3339), clirun.Clip(p.lastError, maxDetail))
	// opencode exits 1 after any error, also one it carried on from (a compacted context
	// overflow): then the Raw goes to the pipeline, which checks the Result and repairs.
	case exit.Err != nil && !p.recovered:
		return raw, p.sessionID, fmt.Errorf("opencode: %w: %s", exit.Err, clirun.Clip(cmp.Or(p.lastError, tail), maxDetail))
	}
	return raw, p.sessionID, nil
}

func writeSession(dir string, s session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("opencode: record the session: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), b, 0o600); err != nil {
		return fmt.Errorf("opencode: record the session: %w", err)
	}
	return nil
}

func readSession(dir string) (session, error) {
	var s session
	b, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		return s, fmt.Errorf("opencode: no run to repair: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("opencode: the session record: %w", err)
	}
	return s, nil
}
