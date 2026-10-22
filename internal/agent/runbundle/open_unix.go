//go:build unix

package runbundle

import (
	"io/fs"
	"syscall"
)

// openNonBlock opens a FIFO without waiting for a writer.
const openNonBlock = syscall.O_NONBLOCK

// links is fi's hard link count.
func links(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // Nlink is uint16 on darwin, uint64 on linux
	}
	return 1
}
