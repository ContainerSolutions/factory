//go:build !unix

package effector

import "os"

// lockFile is a no-op where flock doesn't exist; the agent runs on Unix only.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
