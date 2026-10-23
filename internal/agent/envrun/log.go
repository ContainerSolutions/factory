package envrun

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// maxLine is the longest line redacted whole; a longer one is cut there, so a script that
// never prints a newline can't hold the log's memory.
const maxLine = 64 << 10

// logWriter is out/environment.log: the scripts' stdout and stderr together, redacted line by
// line, capped at MaxLog (then one [truncated] line). It keeps the current script's last
// MaxTail redacted bytes for its error, past the cap too. Safe for concurrent writes (a
// script's stdout and stderr).
type logWriter struct {
	mu        sync.Mutex
	f         *os.File
	redact    func(string) string
	max       int64 // MaxLog; tests lower it
	written   int64
	truncated bool
	line      []byte // the current line, before its newline
	tail      []byte
}

func openLog(path string, max int64, redact func(string) string) (*logWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the environment log: %w", err)
	}
	return &logWriter{f: f, max: max, redact: redact}, nil
}

// begin starts a script's section of the log.
func (w *logWriter) begin(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = w.tail[:0]
	w.emit([]byte("# environment." + name + "\n"))
}

// end flushes the script's last line and returns its tail.
func (w *logWriter) end() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLine()
	return string(w.tail)
}

// Write implements io.Writer.
func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.line = append(w.line, p...)
			if len(w.line) >= maxLine {
				w.flushLine()
			}
			break
		}
		w.line = append(w.line, p[:i]...)
		w.flushLine()
		p = p[i+1:]
	}
	return n, nil
}

// flushLine redacts the current line and emits it.
func (w *logWriter) flushLine() {
	if len(w.line) == 0 {
		return
	}
	line := w.redact(string(w.line)) + "\n"
	w.line = w.line[:0]
	w.tail = append(w.tail, line...)
	if over := len(w.tail) - MaxTail; over > 0 {
		for over < len(w.tail) && w.tail[over]&0xC0 == 0x80 {
			over++ // on a rune boundary
		}
		w.tail = append(w.tail[:0], w.tail[over:]...)
	}
	w.emit([]byte(line))
}

// emit writes b to the file, within max.
func (w *logWriter) emit(b []byte) {
	if w.truncated {
		return
	}
	if w.written+int64(len(b)) > w.max {
		w.truncated = true
		_, _ = w.f.Write([]byte("[truncated]\n"))
		return
	}
	n, _ := w.f.Write(b)
	w.written += int64(n)
}

// Close flushes the last line and closes the file.
func (w *logWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLine()
	return w.f.Close()
}
