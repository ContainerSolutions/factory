package katahelper

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// created is a host with workspace ws created (an empty disk), its calls cleared.
func created(t *testing.T) *fakeHost {
	t.Helper()
	f := newHost(t)
	if code, _, errOut := runIn(t, f.opts(), nil, "create", ws, "5", "8192", "60", "empty", tag); code != 0 {
		t.Fatal(errOut)
	}
	f.calls = nil
	return f
}

// loadInto makes nerdctl exec docker load read its stdin into got, or fail after reading
// nothing.
func loadInto(got *strings.Builder, fail bool) func(io.Reader, io.Writer, io.Writer) error {
	return func(in io.Reader, out, errOut io.Writer) error {
		if fail {
			_, _ = io.WriteString(errOut, "docker load: unexpected EOF\n")
			return fakeExit(1)
		}
		if _, err := io.Copy(got, in); err != nil {
			return err
		}
		_, err := io.WriteString(out, "Loaded image: postgres:17.5\n")
		return err
	}
}

// TestWarmLoad: the host pulls the ref (no unpack, amd64 only), and its save is streamed into
// docker load in the workspace; both their outputs go to stderr (concurrently: -race).
func TestWarmLoad(t *testing.T) {
	f := created(t)
	var got strings.Builder
	f.exec = loadInto(&got, false)
	code, out, errOut := runIn(t, f.opts(), nil, "warm-load", ws, "postgres:17.5@sha256:ab")
	// the save's and the load's output, in either order.
	if lines := strings.SplitAfter(errOut, "\n"); code != 0 || out != "" || len(lines) != 3 ||
		!slices.Equal(slices.Sorted(slices.Values(lines[:2])), []string{"Loaded image: postgres:17.5\n", "saving postgres:17.5@sha256:ab\n"}) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got.String() != "image tar of postgres:17.5@sha256:ab" {
		t.Errorf("docker load read %q", got.String())
	}
	pull := "nerdctl pull --platform linux/amd64 --unpack=false postgres:17.5@sha256:ab"
	// the save and the load run at once: their order is either.
	rest := []string{"nerdctl exec -i " + ws + " docker load", "nerdctl save --platform linux/amd64 postgres:17.5@sha256:ab"}
	if len(f.calls) != 3 || f.calls[0] != pull || !slices.Equal(slices.Sorted(slices.Values(f.calls[1:])), rest) {
		t.Errorf("calls %q", f.calls)
	}
}

// TestWarmLoadFails: a failed pull loads nothing; a failed load stops the save; a failed save
// fails the load.
func TestWarmLoadFails(t *testing.T) {
	t.Run("pull", func(t *testing.T) {
		f := created(t)
		f.fail = "pull"
		code, _, errOut := runIn(t, f.opts(), nil, "warm-load", ws, "postgres:17.5")
		if code != 1 || !strings.HasSuffix(errOut, "kata-helper warm-load: nerdctl pull: exit status 1: boom\n") || len(f.calls) != 1 {
			t.Errorf("code %d, stderr %q, calls %q", code, errOut, f.calls)
		}
	})
	t.Run("load", func(t *testing.T) {
		f := created(t)
		var got strings.Builder
		f.exec = loadInto(&got, true)
		code, _, errOut := runIn(t, f.opts(), nil, "warm-load", ws, "postgres:17.5")
		if code != 1 || !strings.HasSuffix(errOut, "kata-helper warm-load: docker load in "+ws+": exit status 1: docker load: unexpected EOF\n") {
			t.Errorf("code %d, stderr %q", code, errOut)
		}
	})
	t.Run("save", func(t *testing.T) {
		f := created(t)
		var got strings.Builder
		f.exec = loadInto(&got, false)
		f.fail = "save"
		code, _, errOut := runIn(t, f.opts(), nil, "warm-load", ws, "postgres:17.5")
		if code != 1 || !strings.Contains(errOut, "nerdctl save: exit status 1: boom") {
			t.Errorf("code %d, stderr %q", code, errOut)
		}
	})
}

// TestWarmPromote: a stopped workspace's disk replaces the warm disk, and rm then removes the
// rest of it; a running one, or one without a disk, is refused and nothing moves.
func TestWarmPromote(t *testing.T) {
	f := created(t)
	d := f.root + DisksDir + "/" + ws + ".img"
	if err := os.WriteFile(d, []byte("formatted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.root+WarmDisk, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runIn(t, f.opts(), nil, "start", ws); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := runIn(t, f.opts(), nil, "warm-promote", ws); code != 1 || !strings.Contains(errOut, ws+" is running: stop it first") {
		t.Errorf("running: code %d, stderr %q", code, errOut)
	}
	for _, args := range [][]string{{"stop", ws}, {"warm-promote", ws}, {"warm-exists"}, {"rm", ws}, {"warm-exists"}} {
		if code, _, errOut := runIn(t, f.opts(), nil, args...); code != 0 {
			t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	if b, err := os.ReadFile(f.root + WarmDisk); err != nil || string(b) != "formatted" {
		t.Errorf("the warm disk: %q %v", b, err)
	}
	if got := f.files(); !slices.Equal(got, []string{WarmDisk}) {
		t.Errorf("files %q", got)
	}
	if code, _, errOut := runIn(t, f.opts(), nil, "warm-promote", ws); code != 1 || !strings.Contains(errOut, ws+"'s disk: ") {
		t.Errorf("a removed workspace: code %d, stderr %q", code, errOut)
	}
	t.Run("no disk", func(t *testing.T) {
		f := created(t)
		if err := os.Remove(f.root + DisksDir + "/" + ws + ".img"); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runIn(t, f.opts(), nil, "warm-promote", ws); code != 1 || !strings.Contains(errOut, ws+"'s disk: ") {
			t.Errorf("code %d, stderr %q", code, errOut)
		}
		if _, err := os.Lstat(f.root + WarmDisk); err == nil {
			t.Error("a warm disk appeared")
		}
	})
}

// TestWarmExists: no warm disk, or one that isn't a regular file, fails.
func TestWarmExists(t *testing.T) {
	f := newHost(t)
	if code, _, errOut := runIn(t, f.opts(), nil, "warm-exists"); code != 1 || errOut != "kata-helper warm-exists: no warm disk "+WarmDisk+"\n" {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	if err := os.MkdirAll(f.root+WarmDisk, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runIn(t, f.opts(), nil, "warm-exists"); code != 1 || !strings.Contains(errOut, "isn't a regular file") {
		t.Errorf("a directory: code %d, stderr %q", code, errOut)
	}
	if len(f.calls) > 0 {
		t.Errorf("calls %q", f.calls)
	}
}

// TestWarmArgs: the warm verbs refuse anything but a workspace name and an image reference,
// before running anything.
func TestWarmArgs(t *testing.T) {
	for _, args := range [][]string{
		{"warm-load", "ws-gw", "postgres:17"},
		{"warm-load", ws, "-q"},
		{"warm-load", ws, "--help"},
		{"warm-load", ws, "postgres;id"},
		{"warm-load", ws, ""},
		{"warm-load", ws, "x" + strings.Repeat("y", 200)},
		{"warm-promote", "ws-1"},
		{"warm-promote", "../x"},
	} {
		f := newHost(t)
		code, _, errOut := runIn(t, f.opts(), nil, args...)
		if code != 2 || !strings.HasPrefix(errOut, "kata-helper "+args[0]+": ") || len(f.calls) > 0 || len(f.files()) > 0 {
			t.Errorf("%q: code %d, stderr %q, calls %v", args, code, errOut, f.calls)
		}
	}
	for _, args := range [][]string{{"warm-load", ws}, {"warm-load", ws, "a", "b"}, {"warm-promote"}, {"warm-exists", "x"}} {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), nil, args...); code != 2 || errOut != Usage+"\n" || len(f.calls) > 0 {
			t.Errorf("%q: code %d, stderr %q", args, code, errOut)
		}
	}
}
