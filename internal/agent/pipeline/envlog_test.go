package pipeline

import (
	"strings"
	"testing"
)

func TestCapEnvironmentLog(t *testing.T) {
	line := strings.Repeat("x", 1023) + "\n"
	tests := []struct {
		name string
		in   string
		want func(string) bool
	}{
		{"short stays", "# environment.up\nok\n", func(s string) bool { return s == "# environment.up\nok\n" }},
		{"long is cut on a line", strings.Repeat(line, MaxEnvironmentLog/len(line)+10), func(s string) bool {
			return len(s) <= MaxEnvironmentLog && strings.HasSuffix(s, "\n"+EnvironmentLogTruncated) &&
				strings.HasPrefix(s, line) && strings.Count(strings.TrimSuffix(s, EnvironmentLogTruncated), "\n")*len(line) == len(s)-len(EnvironmentLogTruncated)
		}},
		{"one long line", strings.Repeat("y", MaxEnvironmentLog+1), func(s string) bool {
			return len(s) <= MaxEnvironmentLog+1 && strings.HasSuffix(s, "y\n"+EnvironmentLogTruncated)
		}},
	}
	for _, tt := range tests {
		if got := capEnvironmentLog(tt.in); !tt.want(got) {
			t.Errorf("%s: got %d bytes ending %q", tt.name, len(got), got[max(0, len(got)-40):])
		}
	}
}
