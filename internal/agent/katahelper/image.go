package katahelper

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/compliance-framework/factory/internal/agent/internal/execfile"
)

// The image verbs (workspace-kata.md#the-image-provisionkata): image-build builds the workspace
// image with the factory's BuildKit from a build context the owner streams in as a tar;
// image-exists and image-rm find and remove a tag. Every tag is ImageRepo:<version>.

const (
	// buildTimeout bounds image-build: the apt prelude and base.sh's downloads and builds.
	buildTimeout = 90 * time.Minute
	// maxContextFiles and maxContextBytes bound image-build's context: provision's five files
	// are about 60 KiB.
	maxContextFiles = 32
	maxContextBytes = 16 << 20
)

// contextNameRE is a build context file's name: the context is flat.
var contextNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validImage checks a tag argument.
func validImage(tag string) error {
	if !imageRE.MatchString(tag) {
		return badArg("image %q isn't %s:<version>", tag, ImageRepo)
	}
	return nil
}

// imageBuild builds tag from the build context on stdin: a tar of regular files at its top
// level, a Dockerfile among them. The helper copies it into a root-only directory first, so
// BuildKit reads nothing the owner can change or point elsewhere. nerdctl's output goes to
// stderr (stdout is the verbs' result).
//
// It holds BuildDir's lock throughout, and first sweeps the contexts a killed image-build left.
func (h *helper) imageBuild(ctx context.Context, tag string) error {
	if err := os.MkdirAll(h.path(BuildDir), 0o700); err != nil {
		return err
	}
	unlock, err := h.buildLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.sweepContexts(); err != nil {
		return err
	}
	dir, err := h.receiveContext()
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }()
	}
	if err != nil {
		return fmt.Errorf("the build context: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	return h.stream(ctx, nil, h.o.Stderr, "nerdctl build", append(append([]string{}, NerdctlGlobal...), "build",
		"--buildkit-host", "unix://"+BuildkitSocket, "--progress", "plain", "--platform", "linux/amd64", "-t", tag, dir)...)
}

// receiveContext reads the build context tar from stdin into a new directory under BuildDir and
// returns it (also with an error, for the caller to remove). Only regular files with plain
// names, at most maxContextFiles of them and maxContextBytes in all, each named once; modes
// are 0755 for an executable and 0644 for the rest.
func (h *helper) receiveContext() (string, error) {
	if h.o.Stdin == nil {
		return "", errors.New("no stdin")
	}
	dir, err := os.MkdirTemp(h.path(BuildDir), "context-")
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(h.o.Stdin)
	var n int
	var total int64
	dockerfile := false
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return dir, err
		}
		if n++; n > maxContextFiles {
			return dir, fmt.Errorf("more than %d files", maxContextFiles)
		}
		if hd.Typeflag != tar.TypeReg || !contextNameRE.MatchString(hd.Name) {
			return dir, fmt.Errorf("%q isn't a regular file at the top level", hd.Name)
		}
		if total += hd.Size; hd.Size < 0 || total > maxContextBytes {
			return dir, fmt.Errorf("more than %d bytes", maxContextBytes)
		}
		mode := fs.FileMode(0o644)
		if hd.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := writeNew(filepath.Join(dir, hd.Name), tr, hd.Size, mode); err != nil {
			return dir, err
		}
		dockerfile = dockerfile || hd.Name == "Dockerfile"
	}
	if !dockerfile {
		return dir, errors.New("no Dockerfile")
	}
	return dir, nil
}

// sweepContexts removes the context-* directories under BuildDir: with its lock held, they're
// a killed image-build's, whose deferred removal never ran.
func (h *helper) sweepContexts() error {
	stale, err := filepath.Glob(filepath.Join(h.path(BuildDir), "context-*"))
	if err != nil {
		return err
	}
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("a stale build context: %w", err)
		}
	}
	return nil
}

// writeNew writes size bytes of r to the new file p (a file of that name already there is an
// error) with exactly mode.
func writeNew(p string, r io.Reader, size int64, mode fs.FileMode) error {
	return execfile.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode, func(f *os.File) error {
		_, err := io.CopyN(f, r, size)
		return errors.Join(err, f.Chmod(mode))
	})
}

// stream runs nerdctl with stdin and stdout, its stderr to the helper's; a failure names what
// and carries stderr's last line.
func (h *helper) stream(ctx context.Context, stdin io.Reader, stdout io.Writer, what string, args ...string) error {
	tail := &tailBuffer{n: 4 << 10}
	err := h.o.Runner.Run(ctx, Env, stdin, stdout, io.MultiWriter(h.o.Stderr, tail), Nerdctl, args...)
	if err == nil {
		return nil
	}
	if l := lastLine(string(tail.b)); l != "" {
		err = fmt.Errorf("%w: %s", err, l)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w (timed out)", err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// imageExists succeeds when tag is in the factory's containerd.
func (h *helper) imageExists(ctx context.Context, tag string) error {
	return h.nerdctl(ctx, nil, "image", "inspect", tag)
}

// imageRm removes tag, never forced: nerdctl refuses while a workspace uses it.
func (h *helper) imageRm(ctx context.Context, tag string) error {
	return h.nerdctl(ctx, nil, "rmi", tag)
}
