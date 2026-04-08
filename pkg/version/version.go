package version

import "fmt"

// Set via -ldflags at build time.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	Artifact  = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("%s (%s) built %s", Version, Commit, BuildTime)
}

// Full returns a version string prefixed with the artifact name.
func Full() string {
	return fmt.Sprintf("%s %s", Artifact, String())
}
