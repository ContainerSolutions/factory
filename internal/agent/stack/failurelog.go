package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The output a failed stack up leaves for the owner: <stateDir>/logs/stack-<project>-<UTC
// timestamp>.log, the tail of make's output (and of the commands that took the stack down),
// readable only by the owner. local-dev's scripts can echo credentials, so the output itself
// never reaches a log line above debug, the Result or the dispatcher: only its path does,
// and the WARN carries its last line, redacted.
const (
	// LogsDir is the directory under the state directory that holds the failure logs.
	LogsDir = "logs"
	// maxOutputLines bounds a failure log's lines (maxOutputTail bounds its bytes).
	maxOutputLines = 400
	// keepFailureLogs is how many failure logs are kept: the newest.
	keepFailureLogs = 20
	// maxLastLine bounds the last line logged at warn.
	maxLastLine = 512
	// failureLogTime is the timestamp in a failure log's name.
	failureLogTime = "20060102T150405Z"
)

// writeFailureLog writes output (its last maxOutputLines lines) to a new failure log under
// stateDir, after a header naming the failure, and prunes the logs to the newest
// keepFailureLogs. It returns the log's path.
func writeFailureLog(stateDir, project, target string, at time.Time, cause error, output string) (string, error) {
	dir := filepath.Join(stateDir, LogsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("the stack logs directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("the stack logs directory: %w", err)
	}
	at = at.UTC()
	content := fmt.Sprintf("# stack up %s (make %s) failed at %s: %v\n%s", project, target, at.Format(time.RFC3339), cause, lastLines(output, maxOutputLines))
	base := "stack-" + project + "-" + at.Format(failureLogTime)
	var path string
	for i := range 10 {
		name := base + ".log"
		if i > 0 {
			name = fmt.Sprintf("%s-%d.log", base, i+1)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("the stack log: %w", err)
		}
		_, werr := f.WriteString(content)
		if err := errors.Join(werr, f.Close()); err != nil {
			_ = os.Remove(p)
			return "", fmt.Errorf("the stack log: %w", err)
		}
		path = p
		break
	}
	if path == "" {
		return "", fmt.Errorf("the stack log: %s*.log: too many logs for one second", base)
	}
	if err := pruneFailureLogs(dir, path, keepFailureLogs); err != nil {
		return path, err
	}
	return path, nil
}

// pruneFailureLogs removes the oldest failure logs in dir (by modification time) beyond the
// newest keep, never current.
func pruneFailureLogs(dir, current string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("pruning the stack logs: %w", err)
	}
	type logFile struct {
		path string
		mod  time.Time
	}
	var logs []logFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "stack-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == current {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		logs = append(logs, logFile{p, info.ModTime()})
	}
	// Newest first; current is always kept and counts as one.
	slices.SortFunc(logs, func(a, b logFile) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(b.path, a.path)
	})
	var errs []error
	for _, l := range logs[min(len(logs), max(keep-1, 0)):] {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pruning the stack logs: %w", err)
	}
	return nil
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	end := len(strings.TrimRight(s, "\n"))
	i := end
	for range n {
		j := strings.LastIndexByte(s[:i], '\n')
		if j < 0 {
			return s
		}
		i = j
	}
	return s[i+1:]
}

// lastLine is the last non-blank line of s, as a terminal shows it (after the last carriage
// return), at most maxLastLine bytes.
func lastLine(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if len(s) > maxLastLine {
		s = strings.ToValidUTF8(s[:maxLastLine], "") + "…"
	}
	return s
}

// secretRE matches what looks like a credential in a line of output: GitHub tokens
// (ghp_, gho_, ghs_, ghu_, ghr_, github_pat_), API keys (sk-…) and bearer tokens.
var secretRE = regexp.MustCompile(`(?i:\bbearer)\s+[^\s'"]+|\b(?:gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|sk-[A-Za-z0-9_-]{8,})`)

// redacted is the replacement of a credential.
const redacted = "[REDACTED]"

// redactLine replaces what looks like a credential in line.
func redactLine(line string) string {
	return secretRE.ReplaceAllStringFunc(line, func(m string) string {
		if len(m) > len("bearer") && strings.EqualFold(m[:len("bearer")], "bearer") {
			return m[:len("bearer")] + " " + redacted
		}
		return redacted
	})
}
