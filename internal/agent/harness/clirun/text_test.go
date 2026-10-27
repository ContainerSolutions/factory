package clirun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
)

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(b []byte) { got = append(got, string(b)) }}
	long := strings.Repeat("x", MaxLine+10)
	for _, chunk := range []string{"{\"a\":", "1}\r\n\n", long, "\nlast"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if len(got) != 3 || got[0] != `{"a":1}` || len(got[1]) != MaxLine || got[2] != "last" {
		t.Errorf("lines: %d, first %q", len(got), got[0])
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if b.String() != "defg" {
		t.Errorf("tail %q", b.String())
	}
}

func TestTranscript(t *testing.T) {
	tr := &Transcript{Name: "h"}
	tr.Add([]byte(`{"type":"x"}`))
	tr.Factory("h-exec", map[string]any{"args": []string{"a"}})
	if got := string(tr.Bytes()); got != "{\"type\":\"x\"}\n{\"args\":[\"a\"],\"type\":\"factory.h-exec\"}\n" {
		t.Errorf("transcript %q", got)
	}
	tr.Add(make([]byte, MaxTranscript))
	if got := string(tr.Bytes()); !strings.HasSuffix(got, "{\"droppedLines\":1,\"type\":\"factory.h-transcript-truncated\"}\n") {
		t.Errorf("no truncation line: %q", got[len(got)-80:])
	}
}

func TestRun(t *testing.T) {
	var lines []string
	exit, err := Run(context.Background(), Process{
		Binary: "/bin/sh", Args: []string{"-c", `read x; echo "$x"; echo two; echo oops >&2; printf last; exit 3`},
		Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}, Stdin: strings.NewReader("one\n"), KillGrace: time.Second,
		Line: func(b []byte) { lines = append(lines, string(b)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"one", "two", "last"}) || exit.Err == nil || exit.Stderr != "oops\n" {
		t.Errorf("lines %q, exit %+v", lines, exit)
	}
	if _, err := Run(context.Background(), Process{Binary: filepath.Join(t.TempDir(), "none")}); err == nil {
		t.Error("a missing binary started")
	}
}

func TestText(t *testing.T) {
	if got := Clip(strings.Repeat("é", 3), 5); got != "éé" {
		t.Errorf("Clip = %q", got)
	}
	if FirstLine(" a\nb ") != "a …" || LastLine("a\nb\n\n") != "b" {
		t.Error("FirstLine/LastLine")
	}
	if !ContainsFold("Rate LIMIT hit", []string{"rate limit"}) || ContainsFold("fine", []string{"rate limit"}) {
		t.Error("ContainsFold")
	}
	for in, want := range map[string]string{"```json\n{\"a\":1}\n```\n": `{"a":1}`, " {} ": "{}", "  ": ""} {
		if got := string(Unfence([]byte(in))); got != want {
			t.Errorf("Unfence(%q) = %q", in, got)
		}
	}
	env := WithEnv([]string{"A=1", "B=2", "A=3"}, "A", "4")
	if !slices.Equal(env, []string{"B=2", "A=4"}) {
		t.Errorf("env %q", env)
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadLimited(p, 3); err != nil || string(b) != "abc" {
		t.Errorf("ReadLimited = %q, %v", b, err)
	}
	rp := RepairPrompt([]string{"/outcome: missing"}, harness.Workspace{Schema: "/w/s.json", Result: "/w/out/result.json"})
	if !strings.Contains(rp, "- /outcome: missing\n") || !strings.Contains(rp, "/w/s.json") || !strings.Contains(rp, "/w/out/result.json") {
		t.Errorf("RepairPrompt %q", rp)
	}
}
