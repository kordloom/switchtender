package audit

import (
	"fmt"
	"time"
)

// SpanClaimType is the spec-owned claim type a span beat entry becomes in a bundle, per the
// LoomSpan profile in loomseal FORMAT.md.
const SpanClaimType = "loomseal.span/1"

// SpanActor and SpanMethod mark a chain entry as a span beat. The chain profile hashes actor,
// method, and path, so the beat's payload rides in those fields and is committed by the chain
// link like any other entry.
const (
	SpanActor  = "span"
	SpanMethod = "SPAN"
)

// SpanPath encodes one beat's payload into the entry path. The encoding is part of what the
// chain hash commits to, so it is fixed: beat in the path, count and cadence as ordered query
// parameters, no escaping needed because every value is a decimal integer.
func SpanPath(beat, count int64, cadenceS int) string {
	return fmt.Sprintf("/span/%d?count=%d&cadence_s=%d", beat, count, cadenceS)
}

// ParseSpanPath decodes a beat path written by SpanPath. It reports ok=false for anything that
// does not round-trip exactly, so a hand-crafted near-miss stays an ordinary entry rather than
// becoming a malformed span claim.
func ParseSpanPath(path string) (beat, count int64, cadenceS int, ok bool) {
	var b, c int64
	var s int
	n, err := fmt.Sscanf(path, "/span/%d?count=%d&cadence_s=%d", &b, &c, &s)
	if err != nil || n != 3 {
		return 0, 0, 0, false
	}
	if SpanPath(b, c, s) != path || b < 1 || c < 0 || s < 1 {
		return 0, 0, 0, false
	}
	return b, c, s, true
}

// NewSpanEntry builds the chain entry for one beat. It is appended like any mutation, so the
// chain commits to the beat the same way it commits to a change.
func NewSpanEntry(at time.Time, beat, count int64, cadenceS int) *Entry {
	return &Entry{
		ID: NewID(), At: at,
		Actor: SpanActor, Method: SpanMethod, Path: SpanPath(beat, count, cadenceS),
	}
}
