// Package scrub owns the pairing between hiding a secret from a reader and recognizing it coming
// back.
//
// Masking a field for a reader is only half of a mechanism. Every object that hides a secret on read
// is also editable, and the only read there is, is the masked one, so a caller who edits anything at
// all submits the mask along with their change. Storing that verbatim writes the mask over the real
// credential: no error, a response that echoes the mask, and every later run failing on a password
// that is now the literal text of a mask. Losing the secret is worse than showing it.
//
// The two halves were written separately three times, once per object, and the third time the second
// half was left out. They are one concept here, and Restore takes the Scrubber rather than repeating
// its logic, so the half that recognizes the mask cannot drift from the half that writes it and
// cannot be written without naming the scrubber it belongs to.
// Two neighboring mechanisms are deliberately not folded in here, because they are not this one.
// A local inventory is masked inside a text format and carries its own marker, which belongs to that
// format rather than to this package. Notification targets are a list whose rows have to be matched
// to stored rows positionally, so recognizing one masked row is a harder question than recognizing a
// masked value. Both keep their own restore, and the shape they share with this one is the three-way
// decision written on Restore rather than the code that makes it.
package scrub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Marker is what a scrubbed secret is replaced with. It is one constant because the writer of a mask
// and the reader of one have to agree, and they agree by construction rather than by two files
// spelling the same string.
const Marker = "[redacted]"

// Scrubber returns a value with its secret-shaped parts masked, leaving the rest readable. It is the
// only thing a caller supplies: what it means to recognize a scrubbed value coming back is decided
// by what it means to produce one.
type Scrubber[T any] interface {
	// Scrub returns v with its secrets replaced by Marker.
	Scrub(v T) T
}

// ScrubberFunc adapts an ordinary function to Scrubber.
type ScrubberFunc[T any] func(v T) T

// Scrub calls f.
func (f ScrubberFunc[T]) Scrub(v T) T { return f(v) }

// Restore returns what an update should store for a field the caller may be echoing back in the form
// they were shown, given the scrubber that produced that form.
//
// Three cases, and they are the same for every scrubbed field:
//
//   - the submission carries no Marker, so it is a real edit and is taken as given
//   - the submission is exactly the scrubbed view of what is stored, so nothing was touched and the
//     stored value is kept
//   - the submission carries a Marker but is not that view, so which secret each Marker stands for
//     cannot be told, and it is refused rather than guessed at or blanked
//
// Comparison is over the JSON encoding, so one rule covers a string command, a map of launch
// variables, and a slice of pipeline steps without an implementation for each. Map keys are sorted by
// the encoder, so two equal maps compare equal whatever order they were built in.
//
// An error means refuse the update and say so. It never means store something.
func Restore[T any](s Scrubber[T], incoming, stored T, field string) (T, error) {
	if !carries(incoming) {
		return incoming, nil
	}
	if sameEncoding(incoming, s.Scrub(stored)) {
		return stored, nil
	}
	var zero T
	return zero, fmt.Errorf("%w: the submitted %s still carries %s, so which secret it stands for "+
		"cannot be told; re-enter the real value, or ask an admin to edit it", ErrEcho, field, Marker)
}

// carries reports whether a value holds Marker anywhere inside it, at any depth.
func carries(v any) bool {
	body, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), Marker)
}

// sameEncoding reports whether two values encode identically.
func sameEncoding(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
