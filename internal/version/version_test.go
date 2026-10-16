package version

import "testing"

func TestString(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	tests := []struct {
		name    string
		version string
		binary  string
		want    string
	}{
		{name: "default dev build", version: "dev", binary: "factory", want: "factory dev"},
		{name: "release build", version: "v0.1.0", binary: "factory-agent", want: "factory-agent v0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version = tt.version
			if got := String(tt.binary); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.binary, got, tt.want)
			}
		})
	}
}
