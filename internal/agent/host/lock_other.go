//go:build !unix

package host

// lockHost does nothing where flock doesn't exist; the agent host runs on Unix only.
func lockHost(*Env) (func(), error) { return func() {}, nil }

// LockSupervise does nothing where flock doesn't exist.
func LockSupervise(*Env) (func(), error) { return func() {}, nil }
