// Package version holds the build-time version string for any binaries
// in this repo (today: just stubfunnel; future cmd helpers wire through
// here). Default is "dev"; release builds override via -ldflags:
//
//	go build -ldflags "-X github.com/CarriedWorldUniverse/bridle/internal/version.Version=v0.1.0" ./stubfunnel
//
// Library consumers (funnel etc) get their version from `go list -m`
// off the imported module — they don't care about this package.
package version

// Version is the build-time version string. Overridden via -ldflags at
// build time; "dev" when unset.
var Version = "dev"
