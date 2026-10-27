package katahelper

import (
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// The helper keeps its own size literals (changing them means reinstalling it as root), but
// create must take every memory the contract lets a task ask for: its minimum is the
// contract's limits.memory minimum, so a task's size is always one create takes.
func TestCreateMinMemoryIsTheContracts(t *testing.T) {
	if want := v1alpha1.WorkspaceMinMemoryGiB << 10; minMemMiB != want {
		t.Errorf("minMemMiB = %d, want v1alpha1.WorkspaceMinMemoryGiB in MiB, %d", minMemMiB, want)
	}
}
