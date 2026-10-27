package host

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/agent/internal/execfile"
)

// MaxArchive bounds what Extract extracts.
const MaxArchive = 512 << 20

// MaxDownload bounds a download: Kata's static release is about 950 MiB. Every download is
// sha256-pinned; the bound only keeps a wrong server from filling the disk.
const MaxDownload = 2 << 30

// Fetcher downloads a URL to a file.
type Fetcher interface {
	Fetch(ctx context.Context, url, dst string) error
}

// HTTPFetcher downloads with an http.Client (default http.DefaultClient).
type HTTPFetcher struct{ Client *http.Client }

// Fetch implements Fetcher: dst must not exist, and the body must be at most MaxDownload.
func (h HTTPFetcher) Fetch(ctx context.Context, url, dst string) error {
	c := h.Client
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, MaxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxDownload {
		err = fmt.Errorf("GET %s: larger than %d bytes", url, MaxDownload)
	}
	return err
}

// fetchVerified downloads url into a new empty temp dir under parent and checks its sha256.
// It returns the file and the temp dir, which the caller removes.
func fetchVerified(ctx context.Context, f Fetcher, url, sum, parent string) (file, dir string, err error) {
	if dir, err = os.MkdirTemp(parent, ".download-"); err != nil {
		return "", "", err
	}
	file = filepath.Join(dir, path.Base(url))
	if err = f.Fetch(ctx, url, file); err == nil {
		err = checkSHA256(file, sum)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return file, dir, nil
}

// fileSHA256 is a file's sha256, hex.
func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkSHA256(p, want string) error {
	got, err := fileSHA256(p)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sha256 of %s is %s, want the pinned %s", filepath.Base(p), got, want)
	}
	return nil
}

// Extract unpacks the .tar.gz at src into dir, which must exist. It refuses absolute paths,
// .. entries, symlinks that point outside dir, entries under a symlink the archive made (so a
// chain of symlinks can't point outside either), entries other than files, dirs and symlinks,
// and more than MaxArchive bytes in all; once extracted, every symlink must resolve inside dir. Writes go through an os.Root, so nothing lands
// outside dir. Modes keep only the 0755 bits.
func Extract(src, dir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(zr)
	var total int64
	// links are the symlinks extracted so far: no entry goes under one, so every entry's
	// parent is a real directory. A target can still go through one, so they're resolved
	// once everything is extracted.
	links := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return checkLinks(root, links)
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("archive entry %q is outside the archive", h.Name)
		}
		for d := filepath.Dir(name); d != "."; d = filepath.Dir(d) {
			if links[d] {
				return fmt.Errorf("archive entry %q is under the symlink %q", h.Name, d)
			}
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(h.Mode) & 0o755
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, mode|0o700)
		case tar.TypeSymlink:
			t := filepath.FromSlash(h.Linkname)
			if filepath.IsAbs(t) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), t)) {
				return fmt.Errorf("archive symlink %q -> %q points outside the archive", h.Name, h.Linkname)
			}
			err = root.Symlink(h.Linkname, name)
			links[name] = true
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > MaxArchive {
				return fmt.Errorf("archive is larger than %d bytes", MaxArchive)
			}
			err = writeEntry(root, name, mode, tr, h.Size)
		default:
			return fmt.Errorf("archive entry %q: unsupported type %q", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

// checkLinks resolves every extracted symlink through root: one whose target goes through
// another symlink and out of the dir (or loops) is an error. A dangling one is fine.
func checkLinks(root *os.Root, links map[string]bool) error {
	for name := range links {
		if _, err := root.Stat(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("archive symlink %q points outside the archive: %w", name, err)
		}
	}
	return nil
}

func writeEntry(root *os.Root, name string, mode fs.FileMode, r io.Reader, size int64) error {
	return execfile.RootOpenFile(root, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600, func(w *os.File) error {
		_, err := io.CopyN(w, r, size)
		return err
	})
}
