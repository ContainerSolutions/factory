// Package workspace is the seam between factory-agent supervise and the VM a task runs in
// (P1 §1, design/workstreams/agent/LLD/supervise.md): a Driver creates, starts, fills, runs,
// empties and destroys one workspace VM per task. The tart driver is in workspace/tart.
package workspace

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"regexp"
)

// Prefix starts every workspace's name. A name is Prefix and the task id (tsk_…), so Phase 0's
// VMs (ws-1, ws-2, ws-gw) are never a workspace: no driver call accepts or lists them.
const Prefix = "ws-"

// The default workspace size (P1 §1: local-dev with kind used 1.9 GiB in the guest).
const (
	DefaultCPU       = 6
	DefaultMemoryMiB = 8 << 10
)

var nameRE = regexp.MustCompile(`^ws-tsk_[A-Za-z0-9_-]{1,64}$`)

// ValidName reports whether name is a workspace's name: ws-tsk_ and up to 64 of
// [A-Za-z0-9_-].
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("workspace name %q isn't %s<task id> (want %s)", name, Prefix, nameRE)
	}
	return nil
}

// Name is the task's workspace name.
func Name(taskID string) (string, error) {
	n := Prefix + taskID
	return n, ValidName(n)
}

// Spec sizes a workspace.
type Spec struct {
	CPU       int
	MemoryMiB int
}

// WithDefaults fills a zero field with the default size.
func (s Spec) WithDefaults() Spec {
	if s.CPU == 0 {
		s.CPU = DefaultCPU
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	return s
}

// Network is a started workspace's addresses: the VM's own, which its gateway pins as its only
// client, and the agent host's address on the VM's network, where the gateway listens.
type Network struct {
	VM, Gateway netip.Addr
}

// Driver runs workspace VMs. Every method takes a workspace name (Name) and refuses any other.
// Everything a VM returns (Exec's output, Get's files) is untrusted.
type Driver interface {
	// Create makes the workspace from the driver's warm base, sized by spec (WithDefaults).
	Create(ctx context.Context, name string, spec Spec) error
	// Start boots it, waits until commands run in it, and returns its addresses.
	Start(ctx context.Context, name string) (Network, error)
	// Put copies the host directory srcDir to dstDir in the VM, replacing it, owned by root and
	// read-only to everyone (files 0444, directories and executables 0555).
	Put(ctx context.Context, name, srcDir, dstDir string) error
	// Exec runs argv in the VM. stdin may be nil. A non-zero exit is an *ExitError. When ctx
	// is done the remote process gets SIGTERM, then SIGKILL after the driver's grace; Exec
	// returns once it has exited, with ctx's error.
	Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
	// Get copies the VM directory srcDir to the host directory dstDir, which must not exist
	// (its parent must). The copy is capped and refuses links out of dstDir and special files.
	Get(ctx context.Context, name, srcDir, dstDir string) error
	// Stop shuts the workspace down and keeps it (--keep-workspace).
	Stop(ctx context.Context, name string) error
	// Destroy stops and deletes it; a workspace that doesn't exist is no error.
	Destroy(ctx context.Context, name string) error
	// List names the driver's workspaces that exist, for the orphan cleanup at start.
	List(ctx context.Context) ([]string, error)
}

// ExitError is Exec's error when the remote command exits non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
