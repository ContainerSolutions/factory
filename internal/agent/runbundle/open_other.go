//go:build !unix

package runbundle

import "io/fs"

// openNonBlock is 0 where there are no FIFOs to block on.
const openNonBlock = 0

// links is 1: the link count isn't available here.
func links(fs.FileInfo) uint64 { return 1 }
