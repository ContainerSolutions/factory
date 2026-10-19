// Package clirun is what the harness adapters (codex, claudecode) share to drive a coding
// agent's CLI: running it in its own process group and killing the whole group when the
// run ends, splitting its stdout into lines for the stream parser, keeping a bounded
// transcript and stderr tail, and the text helpers their parsers need (rate-limit resets,
// versions, summaries).
package clirun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Bounds on what one harness process may produce.
const (
	// MaxLine is one stdout line; the rest of a longer one is dropped.
	MaxLine = 16 << 20
	// MaxTranscript is the transcript kept in memory (the pipeline caps it gzipped).
	MaxTranscript = 64 << 20
	// MaxStderr is the tail of stderr kept.
	MaxStderr = 64 << 10
)

// Commands runs the short commands an adapter's Probe needs (`--version`, a login status);
// tests replace it (or the binary).
type Commands interface {
	// Output runs name with args and env and returns its combined output.
	Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// ExecCommands runs real programs.
type ExecCommands struct{}

// Output implements Commands.
func (ExecCommands) Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Process is one harness invocation.
type Process struct {
	Binary string
	Args   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	// KillGrace is how long the harness gets to exit after SIGTERM before it is SIGKILLed.
	KillGrace time.Duration
	// Line gets each stdout line, without its newline, in order, from one goroutine.
	Line func([]byte)
}

// Exit is how a Process ended.
type Exit struct {
	// Err is the wait error: nil when the harness exited 0, also when a command it left
	// running held its output open past KillGrace.
	Err error
	// Stderr is the last MaxStderr bytes of its stderr.
	Stderr string
}

// Run runs p to its end. The harness leads its own process group: when ctx ends the whole
// group gets SIGTERM, the harness SIGKILL after KillGrace, and whatever is left of the group
// SIGKILL once the harness has exited (always, so commands it left running don't outlive the
// run). The error is only a failure to start; how the harness ended is in Exit.
func Run(ctx context.Context, p Process) (Exit, error) {
	lw := &lineWriter{fn: p.Line}
	if lw.fn == nil {
		lw.fn = func([]byte) {}
	}
	stderr := &tailBuffer{max: MaxStderr}
	cmd := exec.CommandContext(ctx, p.Binary, p.Args...)
	cmd.Dir = p.Dir
	cmd.Env = p.Env
	cmd.Stdin = p.Stdin
	cmd.Stdout, cmd.Stderr = lw, stderr
	inOwnGroup(cmd)
	cmd.Cancel = func() error { return terminateGroup(cmd.Process.Pid) }
	cmd.WaitDelay = p.KillGrace
	if err := cmd.Start(); err != nil {
		return Exit{}, fmt.Errorf("start: %w", err)
	}
	waitErr := cmd.Wait()
	_ = killGroup(cmd.Process.Pid)
	if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		waitErr = nil // the harness exited 0; a command it left behind held its output open
	}
	lw.flush()
	return Exit{Err: waitErr, Stderr: stderr.String()}, nil
}

// lineWriter splits what is written to it into lines (without the newline) for fn; a line
// over MaxLine is cut there and the rest of it dropped.
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
			room := MaxLine - len(w.buf)
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
