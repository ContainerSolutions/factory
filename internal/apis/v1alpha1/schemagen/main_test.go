package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// committed is the repo's schemas/ directory, relative to this package.
const committed = "../../../../schemas"

// TestCommittedSchemasAreFresh runs schemagen twice into a scratch directory and requires
// the output to be byte-identical across runs and to the committed schemas/.
func TestCommittedSchemasAreFresh(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "stale.schema.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{first, second} {
		if err := run(dir); err != nil {
			t.Fatal(err)
		}
	}
	got := readDir(t, first)
	if _, ok := got["stale.schema.json"]; ok {
		t.Error("stale schema not removed")
	}
	again := readDir(t, second)
	want := readDir(t, committed)
	if len(got) != len(want) || len(got) != len(again) {
		t.Errorf("generated %d schemas (%d on the second run), %d committed: run make schemas", len(got), len(again), len(want))
	}
	for name, b := range got {
		if !bytes.Equal(b, again[name]) {
			t.Errorf("%s differs between runs", name)
		}
		if !bytes.Equal(b, want[name]) {
			t.Errorf("schemas/%s is stale: run make schemas", name)
		}
	}
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}
