//go:build !unix

package ca

import "io/fs"

// ownedByMe can't read an owner here; the mode check still applies.
func ownedByMe(fs.FileInfo) bool { return true }
