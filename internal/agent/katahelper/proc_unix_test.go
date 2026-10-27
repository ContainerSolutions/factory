//go:build unix

package katahelper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestExecRunnerGroup: when ctx ends, the command's whole process group is killed (a child it
// started in the background too, which would otherwise hold its output open for killWait); a
// command given an *os.File stdin stays in the helper's group.
func TestExecRunnerGroup(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	t.Run("killed with its group", func(t *testing.T) {
		pidFile := t.TempDir() + "/pid"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			var out bytes.Buffer
			done <- ExecRunner{}.Run(ctx, Env, nil, &out, io.Discard, sh, "-c", `sleep 60 & echo $! > "$0"; wait`, pidFile)
		}()
		var pid int
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
				if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
					t.Fatal(err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the command didn't start its child")
			}
		}
		start := time.Now()
		cancel()
		if err := <-done; err == nil {
			t.Error("a cancelled command succeeded")
		}
		if d := time.Since(start); d >= killWait {
			t.Errorf("Run returned after %s: the child held the output open", d)
		}
		for deadline := time.Now().Add(5 * time.Second); !gone(pid); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the background child %d outlived the command", pid)
			}
		}
	})
	t.Run("stdin a file", func(t *testing.T) {
		in, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = in.Close() }()
		var out bytes.Buffer
		// $$ is the shell's pid; the group's id is in /proc, or ps where there's no /proc.
		script := `cut -d' ' -f5 /proc/$$/stat 2>/dev/null || ps -o pgid= -p $$`
		if err := (ExecRunner{}).Run(context.Background(), Env, in, &out, io.Discard, sh, "-c", script); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(out.String()), strconv.Itoa(syscall.Getpgrp()); got != want {
			t.Errorf("the command's process group %q, want the helper's %s", got, want)
		}
	})
}

// gone reports whether pid has exited (a zombie waiting for its reaper counts).
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && strings.Contains(string(b), ") Z ")
}
