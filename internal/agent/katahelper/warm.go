package katahelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// The warm verbs (workspace-kata.md#the-warm-disk): host init's warm step loads images into a
// build workspace with warm-load, and makes its disk the warm disk with warm-promote, which
// create copies for a warm workspace; warm-exists says whether there is one.

// warmLoadTimeout bounds warm-load's pull, and its save and load: a kind node image is about
// 1 GB.
const warmLoadTimeout = 30 * time.Minute

// warmPlatform is the only platform the workspaces run.
const warmPlatform = "linux/amd64"

// validRef checks a warm image reference as the config does (config.ValidWarmImage).
func validRef(ref string) error {
	if !config.ValidWarmImage(ref) {
		return badArg("image %q isn't an image reference", ref)
	}
	return nil
}

// warmLoad pulls ref on the host, into the factory's containerd, and streams nerdctl save into
// docker load in the workspace, which needs no route out. The workspace must be running; the
// pull's and the load's output go to stderr.
func (h *helper) warmLoad(ctx context.Context, name, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, warmLoadTimeout)
	defer cancel()
	g := NerdctlGlobal
	if err := h.stream(ctx, nil, h.o.Stderr, "nerdctl pull", append(append([]string{}, g...), "pull", "--platform", warmPlatform, "--unpack=false", ref)...); err != nil {
		return err
	}
	// a load that fails stops the save, which would otherwise block on the full pipe.
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	pr, pw := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		err := h.stream(sctx, nil, pw, "nerdctl save", append(append([]string{}, g...), "save", "--platform", warmPlatform, ref)...)
		_ = pw.CloseWithError(err)
		saved <- err
	}()
	lerr := h.stream(ctx, pr, h.o.Stderr, "docker load in "+name, append(append([]string{}, g...), "exec", "-i", name, "docker", "load")...)
	if lerr != nil {
		stop()
	}
	_ = pr.Close()
	serr := <-saved
	if lerr != nil && serr != nil && errors.Is(sctx.Err(), context.Canceled) {
		serr = nil // stopped because the load failed
	}
	return errors.Join(serr, lerr)
}

// warmPromote makes the stopped workspace's disk the warm disk: one rename, which replaces an
// older warm disk atomically, so there's never a moment without one (a create copying the
// older one keeps its open file). The workspace keeps its container and slot, for rm.
func (h *helper) warmPromote(ctx context.Context, name string) error {
	running, err := h.running(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("%s is running: stop it first", name)
	}
	src := h.path(disk(name))
	if fi, err := os.Lstat(src); err != nil {
		return fmt.Errorf("%s's disk: %w", name, err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s's disk %s isn't a regular file", name, disk(name))
	}
	return os.Rename(src, h.path(WarmDisk))
}

// warmExists succeeds when the warm disk is there.
func (h *helper) warmExists() error {
	fi, err := os.Lstat(h.path(WarmDisk))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("no warm disk %s", WarmDisk)
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s isn't a regular file", WarmDisk)
	}
	return nil
}
