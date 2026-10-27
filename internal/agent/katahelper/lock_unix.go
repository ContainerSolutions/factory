//go:build unix

package katahelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lock takes the exclusive flock on LockFile, waiting at most a minute; the returned func
// releases it.
func (h *helper) lock(ctx context.Context) (func(), error) {
	p := h.path(LockFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // a file descriptor fits an int
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", p, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for %s: %w", p, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// errBuildBusy is image-build's error while another one holds BuildDir.
var errBuildBusy = errors.New("another image-build is running")

// buildLock takes the exclusive flock on BuildDir itself, without waiting: an image-build holds
// it while it sweeps the dir and builds, so it never sweeps a running one's context. The
// returned func releases it.
func (h *helper) buildLock() (func(), error) {
	p := h.path(BuildDir)
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errBuildBusy
		}
		return nil, fmt.Errorf("flock %s: %w", p, err)
	}
	return func() { _ = f.Close() }, nil
}
