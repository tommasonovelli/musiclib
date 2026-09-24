// Package failpoint is the one mechanism of the named failpoints of
// DESIGN.md §12.2: "Inserire failpoint nominati nel protocollo e testare
// arresti reali del processo".
//
// A component that has failpoints holds a Hook, nil in production, and
// calls Hit (or HitFile) at each of its named points. A test gives the
// component a Hook that returns an error to inject there, runs a callback
// (to change the disk or the database at an exact moment), or kills the
// process for a real crash (internal/faulttest.Crash, in a child process).
//
// There is no package-level state: the hook belongs to the component that
// a test built, so no test can leak a hook into another. A nil Hook costs
// one comparison per point, and the package imports nothing that could
// change the behaviour of the production binary. The catalogue of the
// points is NOTES.md N-142.
package failpoint

import "os"

// Hook runs at every named point of the component holding it and returns
// the error to inject there, or nil to go on. A component may call it from
// several goroutines at once (the builder's and the importer's workers):
// a test's hook must be safe for concurrent use.
type Hook func(Point) error

// Point is where a component is when it calls its hook.
type Point struct {
	// Name is the point's stable name, snake_case, unique within the
	// component (NOTES.md N-142).
	Name string
	// Path is what the point concerns, relative to the component's root
	// (never absolute), or "".
	Path string
	// File is the open file the point concerns, or nil. A test may write
	// through it to damage a copy at an exact moment.
	File *os.File
}

// Hit runs h at the point name. A nil Hook returns nil.
func (h Hook) Hit(name string) error {
	if h == nil {
		return nil
	}
	return h(Point{Name: name})
}

// HitFile runs h at the point name about path and its open file f (nil if
// none). A nil Hook returns nil.
func (h Hook) HitFile(name, path string, f *os.File) error {
	if h == nil {
		return nil
	}
	return h(Point{Name: name, Path: path, File: f})
}
