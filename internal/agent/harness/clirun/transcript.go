package clirun

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Transcript collects a harness's stream lines (JSON Lines) and the adapter's own
// factory.* lines, up to MaxTranscript. Past that, lines are counted and dropped, and Bytes
// ends with a factory.<Name>-transcript-truncated line. Safe for concurrent use.
type Transcript struct {
	// Name prefixes the truncation line's kind: the harness (codex, claude).
	Name string

	mu      sync.Mutex
	b       bytes.Buffer
	dropped int
}

// Add appends one line.
func (t *Transcript) Add(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.b.Len()+len(line)+1 > MaxTranscript {
		t.dropped++
		return
	}
	t.b.Write(line)
	t.b.WriteByte('\n')
}

// Factory appends the adapter's own line: {"type": "factory.<kind>", fields…}.
func (t *Transcript) Factory(kind string, fields map[string]any) {
	m := map[string]any{"type": "factory." + kind}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	t.Add(b)
}

// Bytes is the transcript so far.
func (t *Transcript) Bytes() []byte {
	t.mu.Lock()
	dropped := t.dropped
	t.mu.Unlock()
	if dropped > 0 {
		t.Factory(t.Name+"-transcript-truncated", map[string]any{"droppedLines": dropped})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.b.Bytes())
}
