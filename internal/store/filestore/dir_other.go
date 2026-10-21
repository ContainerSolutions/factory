//go:build !unix

package filestore

import (
	"errors"
	"io/fs"
	"os"
)

// lockDir: the file store needs flock, which the dispatcher has on its targets (darwin, linux).
func lockDir(string) (*os.File, error) {
	return nil, errors.New("filestore: not supported on this platform (it needs flock)")
}

func owner(fs.FileInfo) (int, bool) { return -1, true }
