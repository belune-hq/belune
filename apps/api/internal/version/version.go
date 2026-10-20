// Package version carries the build's version string.
//
// It exists because the value was previously a literal in main.go, which meant
// it only changed when someone remembered to change it — and nobody did: it
// still read v0.0.11-alpha some twenty-odd releases later, so every trace this
// process exported was labelled with the wrong service version.
//
// A variable set at link time cannot go stale the same way: a build that does
// not set it says "dev", which is true, rather than confidently naming a
// release it is not.
package version

// Version is overridden at build time with:
//
//	go build -ldflags "-X github.com/belune-hq/belune/internal/version.Version=v0.1.0"
var Version = "dev"
