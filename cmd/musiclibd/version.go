package main

import (
	"fmt"
	"io"
	"log/slog"

	"musiclib/internal/buildinfo"
	"musiclib/internal/render"
)

// printVersion is `musiclibd version` (NOTES.md N-325): the application
// version stamped at build time and the renderer's render_version (§2.1).
// It reads no environment, takes no lock and needs neither the database
// nor the volumes.
func printVersion(stdout io.Writer, log *slog.Logger) int {
	if _, err := fmt.Fprintf(stdout, "version: %s\nrender_version: %s\n", buildinfo.Version, render.Version); err != nil {
		logFatal(log, &bootError{code: "version_output", msg: "cannot write the version", err: err})
		return exitFailure
	}
	return exitOK
}
