// Package migrations holds the versioned SQL schema (DESIGN.md §2.3, §4.2),
// embedded so that the binary carries the schema it was built for.
// store.Migrate applies it forward only.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
