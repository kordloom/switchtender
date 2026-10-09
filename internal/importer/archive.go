package importer

import "fmt"

// archiveRefusal is a refusal an archive reader wrote for the person who built the archive. It
// keeps the reader's sentence as its text and reports itself as ErrArchive under errors.Is, so the
// sentence reaches that person unchanged.
type archiveRefusal struct {
	// err carries the sentence and whatever it wrapped.
	err error
}

// Error returns the reader's sentence.
func (e archiveRefusal) Error() string { return e.err.Error() }

// Unwrap returns what the sentence wrapped, so a cause inside it still matches.
func (e archiveRefusal) Unwrap() error { return e.err }

// Is reports whether target is ErrArchive.
func (e archiveRefusal) Is(target error) bool { return target == ErrArchive }

// refuseArchive wraps a reader's refusal so the server can tell it from a failure of its own.
func refuseArchive(format string, args ...any) error {
	return archiveRefusal{err: fmt.Errorf(format, args...)}
}
