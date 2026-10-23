//go:build !unix

package pipeline

import "os"

// openRootDir opens dir as a directory, refusing a symlink (errRootNotDir). Without
// O_NOFOLLOW it compares the opened file with the Lstat, so a Root swapped in between is
// refused.
func openRootDir(dir string) (*os.File, error) {
	lst, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !lst.IsDir() {
		return nil, errRootNotDir
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !os.SameFile(lst, st) {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errRootNotDir
	}
	return f, nil
}
