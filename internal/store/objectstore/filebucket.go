package objectstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/store/journal"
)

// tmpDir is where FileBucket writes an object before it moves it into place. Object names
// never start with '.', so it is never listed.
const tmpDir = ".tmp"

// p2Files are what a P2 state dir (store/filestore) holds. Its transcripts/ has the same
// naming as the object store's, so the two must never share a directory: the object store
// would see an empty log, and its orphan scan would delete P2's transcripts a day later.
var p2Files = []string{journal.FileStoreSnapshot, journal.FileStoreJournal, journal.FileStoreLock}

// FileBucket is a Bucket on a local directory, for development and for tests with two
// dispatchers on one machine. Create writes a tmp file, fsyncs it and links it into place
// (link fails when the name is taken, so the object appears whole or not at all), then fsyncs
// the directory. The directory is private (0700, files 0600), like a P2 state dir.
type FileBucket struct {
	dir string
}

var _ Bucket = (*FileBucket)(nil)

// NewFileBucket opens (creating it if missing) the directory dir as a bucket. It refuses a
// relative path, a directory group or others can access, and a P2 state dir (snapshot.json,
// journal.jsonl or LOCK in it).
func NewFileBucket(dir string) (*FileBucket, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("objectstore: file bucket %q: the path must be absolute", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("objectstore: create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("objectstore: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("objectstore: %s has mode %04o, which group or others can access: run chmod 700 %s", dir, fi.Mode().Perm(), dir)
	}
	for _, f := range p2Files {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			return nil, fmt.Errorf("objectstore: %s holds %s, so it is a P2 state dir (--state-dir %s): a file:// bucket needs a directory of its own", dir, f, dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("objectstore: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), 0o700); err != nil {
		return nil, fmt.Errorf("objectstore: create %s: %w", tmpDir, err)
	}
	return &FileBucket{dir: dir}, nil
}

// Dir returns the bucket's directory.
func (b *FileBucket) Dir() string { return b.dir }

// path returns name's file, refusing a name that could leave the directory or reach the tmp
// dir.
func (b *FileBucket) path(name string) (string, error) {
	segs := strings.Split(name, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsRune(s, os.PathSeparator) {
			return "", fmt.Errorf("objectstore: bad object name %q", name)
		}
	}
	if segs[0] == tmpDir {
		return "", fmt.Errorf("objectstore: bad object name %q", name)
	}
	return filepath.Join(b.dir, filepath.FromSlash(name)), nil
}

// Get implements Bucket.
func (b *FileBucket) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := b.path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotExist)
	}
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", name, err)
	}
	return data, nil
}

// Create implements Bucket.
func (b *FileBucket) Create(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := b.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("objectstore: create the directory of %s: %w", name, err)
	}
	f, err := os.CreateTemp(filepath.Join(b.dir, tmpDir), "obj-*")
	if err != nil {
		return fmt.Errorf("objectstore: write %s: %w", name, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint:errcheck // the linked name keeps the data
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("objectstore: write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("objectstore: fsync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("objectstore: close %s: %w", name, err)
	}
	if err := os.Link(tmp, p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", name, ErrExists)
		}
		return fmt.Errorf("objectstore: create %s: %w", name, err)
	}
	return syncDir(filepath.Dir(p))
}

// List implements Bucket.
func (b *FileBucket) List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	base := b.dir
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		p, err := b.path(prefix[:i])
		if err != nil {
			return err
		}
		base = p
	}
	var objs []ObjectInfo
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // the prefix's directory doesn't exist (yet), or vanished
			}
			return err
		}
		rel, err := filepath.Rel(b.dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			if name == tmpDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(name, prefix) || name <= startAfter || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted since the directory was read
		}
		if err != nil {
			return err
		}
		objs = append(objs, ObjectInfo{Name: name, Size: fi.Size(), Updated: fi.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("objectstore: list %s: %w", prefix, err)
	}
	slices.SortFunc(objs, func(a, b ObjectInfo) int { return cmp.Compare(a.Name, b.Name) })
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// Delete implements Bucket.
func (b *FileBucket) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := b.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", name, ErrNotExist)
	} else if err != nil {
		return fmt.Errorf("objectstore: delete %s: %w", name, err)
	}
	return nil
}

// syncDir fsyncs a directory, so the links and creates in it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("objectstore: open %s for fsync: %w", dir, err)
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("objectstore: fsync %s: %w", dir, err)
	}
	return nil
}
