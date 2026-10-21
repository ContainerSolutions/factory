package filestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// checkTree refuses a state dir that isn't private: the dir, the files in it and in
// transcripts/ must have no group or other permission bits and belong to this user. It
// names the fix.
func checkTree(dir string) error {
	if err := checkPrivate(dir); err != nil {
		return err
	}
	for _, d := range []string{dir, filepath.Join(dir, transcriptDir)} {
		entries, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("filestore: read %s: %w", d, err)
		}
		for _, e := range entries {
			if err := checkPrivate(filepath.Join(d, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPrivate(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	mode := fi.Mode()
	if mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("filestore: %w: %s is a symlink; the state dir holds its own files only", ErrInsecure, path)
	}
	want := fs.FileMode(0o600)
	if mode.IsDir() {
		want = 0o700
	}
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("filestore: %w: %s has mode %04o, which group or others can access: run chmod %o %s",
			ErrInsecure, path, mode.Perm(), want, path)
	}
	if uid, mine := owner(fi); !mine {
		return fmt.Errorf("filestore: %w: %s belongs to uid %d, not this user (uid %d): chown it or use another state dir",
			ErrInsecure, path, uid, os.Geteuid())
	}
	return nil
}
