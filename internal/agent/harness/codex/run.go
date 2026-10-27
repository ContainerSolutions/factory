package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// Bounds on what one codex run may produce besides its stream (clirun bounds that).
const (
	maxMessage = 1 << 20 // the final-message file
	maxDetail  = 300     // error text in an error
)

// session is what Repair needs from the Run: the thread to resume and the settings to run it
// with, the sandbox and working directory included, so a repair resumes where the run ran.
// It lives in the scratch CODEX_HOME, so the Adapter holds no per-run state.
type session struct {
	ThreadID string `json:"threadId,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Sandbox  string `json:"sandbox"`
	WorkDir  string `json:"workDir"`
}

// workDir is where codex runs: the pipeline's WorkDir, else the workspace root.
func workDir(ws harness.Workspace) string {
	if ws.WorkDir != "" {
		return ws.WorkDir
	}
	return ws.Root
}

// Run implements harness.Adapter: `codex exec --json` on the prompt (stdin) with the task's
// resolved model and effort, danger-full-access (the VM is the boundary) from workDir's
// directory.
func (a *Adapter) Run(ctx context.Context, ws harness.Workspace, t v1alpha1.Task, prompt string, sink harness.EventSink) (harness.Raw, error) {
	s := session{Sandbox: sandboxDangerFullAccess, WorkDir: workDir(ws), Model: a.o.Config.Default}
	if s.WorkDir == "" {
		return harness.Raw{}, errors.New("codex: the workspace has neither a work dir nor a root to run in")
	}
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		if h.Resolved.Model != "" {
			s.Model = h.Resolved.Model
		}
		s.Effort = h.Resolved.ReasoningEffort
	}
	home := codexHome(ws)
	if err := os.WriteFile(filepath.Join(home, promptFile), []byte(prompt), 0o600); err != nil {
		return harness.Raw{}, fmt.Errorf("codex: keep the prompt: %w", err)
	}
	raw, threadID, err := a.exec(ctx, ws, s, prompt, sink)
	s.ThreadID = threadID
	if werr := writeSession(home, s); werr != nil && err == nil {
		err = werr
	}
	return raw, err
}

// Repair implements harness.Adapter: the same session resumed (`codex exec resume <thread>`)
// with the problems, asking for a valid Result only. Without a thread to resume, a new exec
// gets the original prompt followed by the problems. The sandbox and working directory are
// the Run's, from the session record.
func (a *Adapter) Repair(ctx context.Context, ws harness.Workspace, problems []string, sink harness.EventSink) (harness.Raw, error) {
	home := codexHome(ws)
	s, err := readSession(home)
	if err != nil {
		return harness.Raw{}, err
	}
	prompt := clirun.RepairPrompt(problems, ws)
	if s.ThreadID == "" {
		orig, err := os.ReadFile(filepath.Join(home, promptFile))
		if err != nil {
			return harness.Raw{}, fmt.Errorf("codex: no session to resume and no prompt: %w", err)
		}
		prompt = string(orig) + "\n\n---\n\n" + prompt
	}
	raw, threadID, err := a.exec(ctx, ws, s, prompt, sink)
	if threadID != "" && threadID != s.ThreadID {
		s.ThreadID = threadID
		_ = writeSession(home, s)
	}
	return raw, err
}

// exec runs one codex process to its end and returns its raw output and thread id.
func (a *Adapter) exec(ctx context.Context, ws harness.Workspace, s session, prompt string, sink harness.EventSink) (harness.Raw, string, error) {
	home := codexHome(ws)
	last := filepath.Join(home, lastMessageFile)
	if err := os.Remove(last); err != nil && !errors.Is(err, os.ErrNotExist) {
		return harness.Raw{}, "", fmt.Errorf("codex: %w", err)
	}
	o := execOptions{Model: s.Model, Effort: s.Effort, Sandbox: s.Sandbox, WorkDir: s.WorkDir, LastMessage: last, Resume: s.ThreadID}
	if fi, err := os.Stat(ws.Schema); err == nil && fi.Size() > 0 {
		o.Schema = ws.Schema
	}
	args := execArgs(o)

	tr := &clirun.Transcript{Name: "codex"}
	tr.Factory("codex-exec", map[string]any{"args": args})
	p := newParser(sink, a.o.Clock, a.o.Throttle)
	exit, err := clirun.Run(ctx, clirun.Process{
		Binary: a.o.Binary, Args: args, Dir: s.WorkDir, Env: clirun.WithEnv(ws.Env, "CODEX_HOME", home),
		Stdin: strings.NewReader(prompt), KillGrace: a.o.KillGrace,
		Line: func(line []byte) { tr.Add(line); p.line(line) },
	})
	if err != nil {
		return harness.Raw{Transcript: tr.Bytes()}, "", fmt.Errorf("codex: %w", err)
	}
	waitErr := exit.Err
	if strings.TrimSpace(exit.Stderr) != "" {
		tr.Factory("codex-stderr", map[string]any{"text": exit.Stderr})
	}
	raw := harness.Raw{Transcript: tr.Bytes(), Result: finalMessage(last, p.lastMessage)}

	switch {
	case ctx.Err() != nil:
		return raw, p.threadID, ctx.Err()
	case p.limited:
		return raw, p.threadID, fmt.Errorf("codex: rate limited until %s: %s", p.until.UTC().Format(time.RFC3339), clirun.Clip(p.lastError, maxDetail))
	case waitErr != nil:
		tail := clirun.LastLine(exit.Stderr)
		if isRateLimit(tail) {
			until := clirun.ResetTime(nil, tail, a.o.Clock.Now(), a.o.Throttle)
			sink.RateLimited(until)
			return raw, p.threadID, fmt.Errorf("codex: rate limited until %s: %s", until.UTC().Format(time.RFC3339), clirun.Clip(tail, maxDetail))
		}
		detail := p.lastError
		if detail == "" {
			detail = tail
		}
		return raw, p.threadID, fmt.Errorf("codex: %w: %s", waitErr, clirun.Clip(detail, maxDetail))
	case p.failed:
		return raw, p.threadID, fmt.Errorf("codex: the turn failed: %s", clirun.Clip(p.lastError, maxDetail))
	}
	return raw, p.threadID, nil
}

// finalMessage is the final message: the --output-last-message file, else the stream's last
// agent message, without a Markdown code fence around it.
func finalMessage(file, streamed string) []byte {
	b, err := clirun.ReadLimited(file, maxMessage)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		b = []byte(streamed)
	}
	return clirun.Unfence(b)
}

func writeSession(home string, s session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, sessionFile), b, 0o600); err != nil {
		return fmt.Errorf("codex: record the session: %w", err)
	}
	return nil
}

func readSession(home string) (session, error) {
	var s session
	b, err := os.ReadFile(filepath.Join(home, sessionFile))
	if err != nil {
		return s, fmt.Errorf("codex: no run to repair: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("codex: the session record: %w", err)
	}
	return s, nil
}
