// Package version holds build metadata, set at link time with
// -ldflags "-X github.com/ContainerSolutions/factory/internal/version.Version=v1.2.3".
package version

// Version is the release version of the binaries; "dev" for local builds.
var Version = "dev"

// String returns the version line printed by the binaries.
func String(binary string) string {
	return binary + " " + Version
}
