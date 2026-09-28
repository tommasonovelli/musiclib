// Package buildinfo holds the application version, its single source: the
// boot's `starting` event, `musiclibd version` and the backup manifest's
// app_version (DESIGN.md §11.4 step 4) all read it from here (NOTES.md
// N-325).
package buildinfo

// Version is the application version. Release images set it at build time:
//
//	go build -ldflags "-X musiclib/internal/buildinfo.Version=1.0.0"
//
// through the Dockerfile's build argument MUSICLIB_VERSION, which the
// build-app stage checks is a non-empty token of [0-9A-Za-z.+-] and that
// the built binary reports it. Any other build, tests included, reports
// "devel". It must never be empty: restore refuses a manifest with an
// empty app_version.
var Version = "devel"
