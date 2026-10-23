package workspace

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	tests := []struct {
		id, want string
		ok       bool
	}{
		{"tsk_0123456789abcdef0123", "ws-tsk_0123456789abcdef0123", true},
		{"tsk_a-b_C", "ws-tsk_a-b_C", true},
		{"tsk_" + strings.Repeat("a", 64), "", true},
		{"tsk_" + strings.Repeat("a", 65), "", false},
		{"tsk_", "", false},
		{"gw", "", false}, // Phase 0's ws-gw
		{"1", "", false},  // Phase 0's ws-1
		{"tsk_a.building", "", false},
		{"tsk_a/b", "", false},
		{"tsk_a b", "", false},
	}
	for _, tt := range tests {
		got, err := Name(tt.id)
		if (err == nil) != tt.ok {
			t.Errorf("Name(%q) err %v, want ok %v", tt.id, err, tt.ok)
		}
		if tt.want != "" && got != tt.want {
			t.Errorf("Name(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestSpecDefaults(t *testing.T) {
	if got := (Spec{}).WithDefaults(); got != (Spec{CPU: 6, MemoryMiB: 8192}) {
		t.Errorf("defaults %+v", got)
	}
	if got := (Spec{CPU: 2}).WithDefaults(); got != (Spec{CPU: 2, MemoryMiB: 8192}) {
		t.Errorf("partial %+v", got)
	}
}
