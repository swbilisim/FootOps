// Package version holds build metadata injected at link time.
package version

// Set via -ldflags "-X github.com/swbilisim/FootOps/internal/version.Version=..." in release builds.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
