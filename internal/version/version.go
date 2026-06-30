// Package version exposes build metadata for the atlas library.
//
// It lives under internal/ so the values can be stamped at build time
// (via -ldflags) without becoming part of the public API surface.
package version

// Set via: go build -ldflags "-X github.com/simplyblock/atlas/internal/version.Version=v1.2.3"
var (
	// Version is the semantic version of the build.
	Version = "dev"
	// Commit is the git SHA the binary was built from.
	Commit = "unknown"
)
