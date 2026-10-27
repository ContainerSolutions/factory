package execfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestWriteFile: a script written while other goroutines keep forking execs at once, every
// time. With a plain os.WriteFile this fails with "text file busy" within a few dozen writes.
func TestWriteFile(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command(sh, "-c", ":").Run()
			}
		})
	}
	defer func() { close(stop); wg.Wait() }()
	for i := range 200 {
		p := filepath.Join(dir, fmt.Sprintf("s%d", i))
		if err := WriteFile(p, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(p).Output()
		if err != nil || string(out) != "ok\n" {
			t.Fatalf("write %d: exec %s: %q, %v", i, p, out, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "s0")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, %v; want 0700", fi, err)
	}
}

// TestForkLock: a write whose mode has an exec bit waits for syscall.ForkLock (a fork in
// progress holds it); one without doesn't.
func TestForkLock(t *testing.T) {
	for _, tt := range []struct {
		name  string
		perm  fs.FileMode
		waits bool
	}{
		{"0600", 0o600, false},
		{"0644", 0o644, false},
		{"0700", 0o700, true},
		{"0755", 0o755, true},
		{"0610 (group exec)", 0o610, true},
		{"0601 (other exec)", 0o601, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			done := make(chan error, 1)
			syscall.ForkLock.Lock()
			go func() { done <- WriteFile(p, []byte("x"), tt.perm) }()
			// A write that must wait is still blocked after 100 ms (at worst a slow write
			// passes falsely); one that mustn't fails only when it is really blocked, so its
			// bound is generous enough for a loaded -race run.
			var err error
			if tt.waits {
				select {
				case werr := <-done:
					syscall.ForkLock.Unlock()
					t.Fatalf("the write finished (%v) while ForkLock was held", werr)
				case <-time.After(100 * time.Millisecond):
				}
				syscall.ForkLock.Unlock()
				err = <-done
			} else {
				select {
				case err = <-done:
					syscall.ForkLock.Unlock()
				case <-time.After(10 * time.Second):
					syscall.ForkLock.Unlock()
					<-done
					t.Fatal("the write waited for ForkLock")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != "x" {
				t.Errorf("read %q, %v; want x", b, err)
			}
		})
	}
}

// TestOpenFile: open's and fill's errors come back as they are; what fill writes is there; a
// root keeps the file inside it.
func TestOpenFile(t *testing.T) {
	errFill := errors.New("fill failed")
	write := func(s string) func(*os.File) error {
		return func(f *os.File) error { _, err := f.WriteString(s); return err }
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	excl := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	for _, tt := range []struct {
		name    string
		do      func() error
		wantErr bool
		is      error  // with wantErr: what the error must match, when set
		file    string // with want: what the file then holds ("" for no check)
		want    string
	}{
		{name: "writes", do: func() error { return OpenFile(filepath.Join(dir, "a"), excl, 0o700, write("a")) }, file: "a", want: "a"},
		{name: "an open error", do: func() error { return OpenFile(filepath.Join(dir, "a"), excl, 0o700, write("b")) },
			wantErr: true, is: fs.ErrExist, file: "a", want: "a"},
		{name: "a fill error", do: func() error {
			return OpenFile(filepath.Join(dir, "c"), excl, 0o700, func(*os.File) error { return errFill })
		}, wantErr: true, is: errFill, file: "c", want: ""},
		{name: "through a root", do: func() error { return RootOpenFile(root, "d", excl, 0o755, write("d")) }, file: "d", want: "d"},
		{name: "a root keeps it inside", do: func() error { return RootOpenFile(root, "../e", excl, 0o755, write("e")) }, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.do()
			if (err != nil) != tt.wantErr || (tt.is != nil && !errors.Is(err, tt.is)) {
				t.Fatalf("error %v, want error %v (matching %v)", err, tt.wantErr, tt.is)
			}
			if tt.file == "" {
				return
			}
			if b, err := os.ReadFile(filepath.Join(dir, tt.file)); err != nil || string(b) != tt.want {
				t.Errorf("%s holds %q (%v), want %q", tt.file, b, err, tt.want)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), "e")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("../e: %v, want no file outside the root", err)
	}
}
