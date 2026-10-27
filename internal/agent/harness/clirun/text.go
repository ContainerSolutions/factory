package clirun

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
)

// Clip cuts s to n bytes on a rune boundary.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// FirstLine is s's first line, with " …" when more follows.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// LastLine is s's last non-empty line.
func LastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// ContainsFold reports whether s contains any of words, ignoring case (words are lower case).
func ContainsFold(s string, words []string) bool {
	m := strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

// Unfence is b trimmed, without a Markdown code fence (and its language tag) around it; nil
// when nothing is left.
func Unfence(b []byte) []byte {
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

// ReadLimited reads at most n bytes of the file at p.
func ReadLimited(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, n))
}

// WithEnv is env with key set to value (any earlier value dropped).
func WithEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// RepairPrompt asks the harness, in the session it ran, for a corrected Result only.
func RepairPrompt(problems []string, ws harness.Workspace) string {
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
