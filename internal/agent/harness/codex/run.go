package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Bounds on what one codex process may produce.
const (
	maxLine       = 16 << 20 // one JSONL line; the rest of a longer one is dropped
	maxTranscript = 64 << 20 // the transcript kept in memory (the pipeline caps it gzipped)
	maxStderr     = 64 << 10 // the tail of stderr kept
	maxMessage    = 1 << 20  // the final-message file
	maxDetail     = 300      // error text in an error
)

// session is what Repair needs from the Run: the thread to resume and the settings to run it
// with. It lives in the scratch CODEX_HOME, so the Adapter holds no per-run state.
type session struct {
	ThreadID string `json:"threadId,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Sandbox  string `json:"sandbox"`
	WorkDir  string `json:"workDir"`
}

// sandboxFor is the sandbox the checkout allows: workspace-write when the harness may write
// the primary checkout, else read-only (the Result then comes back as the final message, the
// sandbox refusing writes to $FACTORY_RESULT).
func sandboxFor(ws harness.Workspace) string {
	if ws.Repo != "" && ws.RepoWritable {
		return sandboxWorkspaceWrite
	}
	return sandboxReadOnly
}

// Run implements harness.Adapter: `codex exec --json` on the prompt (stdin) with the task's
// resolved model and effort, in the workspace's working directory.
func (a *Adapter) Run(ctx context.Context, ws harness.Workspace, t v1alpha1.Task, prompt string, sink harness.EventSink) (harness.Raw, error) {
	s := session{Sandbox: sandboxFor(ws), WorkDir: ws.WorkDir, Model: a.o.Config.Default}
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		if h.Resolved.Model != "" {
			s.Model = h.Resolved.Model
		}
		s.Effort = h.Resolved.ReasoningEffort
	}
	if s.WorkDir == "" {
		s.WorkDir = ws.Root
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
// gets the original prompt followed by the problems.
func (a *Adapter) Repair(ctx context.Context, ws harness.Workspace, problems []string, sink harness.EventSink) (harness.Raw, error) {
	home := codexHome(ws)
	s, err := readSession(home)
	if err != nil {
		return harness.Raw{}, err
	}
	prompt := repairPrompt(problems, ws)
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

// repairPrompt asks for a corrected Result only.
func repairPrompt(problems []string, ws harness.Workspace) string {
	var b strings.Builder
	b.WriteString("The Result you produced is invalid:\n\n")
	for _, p := range problems {
		b.WriteString("- " + p + "\n")
	}
	b.WriteString("\nFix only the Result. Do not change anything else and do not repeat any other action. ")
	fmt.Fprintf(&b, "Write the corrected Result as one JSON object, matching the schema in %s, to $FACTORY_RESULT (%s), ", ws.Schema, ws.Result)
	b.WriteString("and give the same JSON object, with nothing around it, as your final message.\n")
	return b.String()
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

	tr := &transcript{}
	tr.factory("codex-exec", map[string]any{"args": args})
	p := newParser(sink, a.o.Clock, a.o.Throttle)
	lw := &lineWriter{fn: func(line []byte) { tr.add(line); p.line(line) }}
	stderr := &tailBuffer{max: maxStderr}

	cmd := exec.CommandContext(ctx, a.o.Binary, args...)
	cmd.Dir = s.WorkDir
	cmd.Env = withEnv(ws.Env, "CODEX_HOME", home)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout, cmd.Stderr = lw, stderr
	inOwnGroup(cmd)
	// On cancel: SIGTERM to the whole group, SIGKILL to codex after the grace (WaitDelay),
	// and to what is left of the group once codex is gone.
	cmd.Cancel = func() error { return terminateGroup(cmd.Process.Pid) }
	cmd.WaitDelay = a.o.KillGrace
	if err := cmd.Start(); err != nil {
		return harness.Raw{Transcript: tr.bytes()}, "", fmt.Errorf("codex: start: %w", err)
	}
	waitErr := cmd.Wait()
	_ = killGroup(cmd.Process.Pid)
	if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		waitErr = nil // codex exited 0; a command it left behind held its output open
	}
	lw.flush()
	if s := stderr.String(); strings.TrimSpace(s) != "" {
		tr.factory("codex-stderr", map[string]any{"text": s})
	}
	raw := harness.Raw{Transcript: tr.bytes(), Result: finalMessage(last, p.lastMessage)}

	switch {
	case ctx.Err() != nil:
		return raw, p.threadID, ctx.Err()
	case p.limited:
		return raw, p.threadID, fmt.Errorf("codex: rate limited until %s: %s", p.until.UTC().Format(time.RFC3339), clip(p.lastError, maxDetail))
	case waitErr != nil:
		tail := lastLine(stderr.String())
		if isRateLimit(tail) {
			until := resetTime(nil, tail, a.o.Clock.Now(), a.o.Throttle)
			sink.RateLimited(until)
			return raw, p.threadID, fmt.Errorf("codex: rate limited until %s: %s", until.UTC().Format(time.RFC3339), clip(tail, maxDetail))
		}
		detail := p.lastError
		if detail == "" {
			detail = tail
		}
		return raw, p.threadID, fmt.Errorf("codex: %w: %s", waitErr, clip(detail, maxDetail))
	case p.failed:
		return raw, p.threadID, fmt.Errorf("codex: the turn failed: %s", clip(p.lastError, maxDetail))
	}
	return raw, p.threadID, nil
}

// finalMessage is the final message: the --output-last-message file, else the stream's last
// agent message, without a Markdown code fence around it.
func finalMessage(file, streamed string) []byte {
	b, err := readLimited(file, maxMessage)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		b = []byte(streamed)
	}
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, []byte("```")) && bytes.HasSuffix(b, []byte("```")) && len(b) > 6 {
		b = b[3 : len(b)-3]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:] // the fence's language tag
		}
		b = bytes.TrimSpace(b)
	}
	if len(b) == 0 {
		return nil
	}
	return b
}

func readLimited(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, n))
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

// withEnv is env with key set to value (any earlier value dropped).
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// transcript collects the stream's lines (JSON Lines) and the adapter's own factory.* lines,
// up to maxTranscript.
type transcript struct {
	mu      sync.Mutex
	b       bytes.Buffer
	dropped int
}

func (t *transcript) add(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.b.Len()+len(line)+1 > maxTranscript {
		t.dropped++
		return
	}
	t.b.Write(line)
	t.b.WriteByte('\n')
}

func (t *transcript) factory(kind string, fields map[string]any) {
	m := map[string]any{"type": "factory." + kind}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	t.add(b)
}

func (t *transcript) bytes() []byte {
	t.mu.Lock()
	dropped := t.dropped
	t.mu.Unlock()
	if dropped > 0 {
		t.factory("codex-transcript-truncated", map[string]any{"droppedLines": dropped})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.b.Bytes())
}

// lineWriter splits what is written to it into lines (without the newline) for fn; a line
// over maxLine is cut there and the rest of it dropped.
type lineWriter struct {
	fn   func([]byte)
	buf  []byte
	skip bool // dropping the rest of an over-long line
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !w.skip {
			room := maxLine - len(w.buf)
			if len(chunk) > room {
				w.buf = append(w.buf, chunk[:room]...)
				w.skip = true
			} else {
				w.buf = append(w.buf, chunk...)
			}
		}
		if i < 0 {
			break
		}
		w.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (w *lineWriter) emit() {
	line := bytes.TrimRight(w.buf, "\r")
	if len(bytes.TrimSpace(line)) > 0 {
		w.fn(line)
	}
	w.buf, w.skip = w.buf[:0], false
}

// flush emits a last line without a newline.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
