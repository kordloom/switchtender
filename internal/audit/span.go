package audit

import (
	"fmt"
	"math"
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

// MethodCLI is the method recorded for a mutation made from the command line, so the trail reads
// the change apart from an HTTP one. It is a state change, counted with the HTTP write methods.
const MethodCLI = "CLI"

// MethodRun is the method recorded for the entry a run's outcome commits when it finishes, so the
// trail reads the record of what a run did apart from the request that asked for it. It is a state
// change, counted with the HTTP write methods.
const MethodRun = "RUN"

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

// IsSpanMarker reports whether the entry is a well-formed span beat: the span actor, the span
// method, and a path that round-trips through ParseSpanPath. The combination is reserved. Every
// reader of the chain, from the beat numbering in AppendSpanBeat to the bundle builder and the
// public feed, treats such an entry as a beat the server minted, so a store must refuse it on any
// path other than AppendSpanBeat or a caller could inject a beat that renumbers the real ones and
// fails every bundle built over the chain. An entry that wears the marker but whose path does not
// round-trip is not a span marker; it stays an ordinary entry everywhere.
func IsSpanMarker(e *Entry) bool {
	if e.Actor != SpanActor || e.Method != SpanMethod {
		return false
	}
	_, _, _, ok := ParseSpanPath(e.Path)
	return ok
}

// NextSpanBeat computes the next beat number and its count from the chain head sequence and the
// newest well-formed span entry's sequence and beat. A lastSpanBeat below one means the chain
// holds no span entry: the first beat is one and its count is every prior entry, which is headSeq
// itself. Otherwise the beat increases by exactly one and the count is the number of entries
// appended after the last span entry.
func NextSpanBeat(headSeq, lastSpanSeq, lastSpanBeat int64) (beat, count int64) {
	if lastSpanBeat < 1 {
		return 1, headSeq
	}
	return lastSpanBeat + 1, headSeq - lastSpanSeq
}

// BeatGranularity is the smallest gap the chain can hold between two beats. A recorded time is
// truncated to microseconds before anything hashes it, so two beats closer together than this land
// on the same stored instant. See Link.
const BeatGranularity = time.Microsecond

// CheckBeatAdvance reports whether a beat carrying at may be appended after the newest beat in the
// chain, which was recorded at prev. It returns a ClockBehindError naming beat and both times when
// the time does not strictly advance. A zero prev means the chain holds no beat yet, so there is
// nothing to advance past.
//
// A verifier fails a bundle outright when a beat's time does not strictly advance past the beat
// before it, unlike a cadence gap, which is only reported with its bounds and duration. Clocks do
// move backward in practice, from an NTP step on a running server, a virtual machine resuming from
// a snapshot, or a restart after an offline correction. The beat is skipped rather than moved
// forward to fit, because a beat's time is a signed claim about when the population was counted:
// writing a time the clock did not read would make the attestation say something false, which is
// the one thing this chain exists to rule out. A skipped beat costs a longer unattested window,
// which the record reports honestly.
func CheckBeatAdvance(at, prev time.Time, beat int64) error {
	if prev.IsZero() {
		return nil
	}
	// Both sides are compared at the granularity the chain stores, since a beat leading the last one
	// by nanoseconds alone is written as the same instant and does not advance.
	at, prev = at.Truncate(BeatGranularity), prev.Truncate(BeatGranularity)
	if at.After(prev) {
		return nil
	}
	return &ClockBehindError{Prev: prev, At: at, Beat: beat}
}

// SpanScanLimit returns how many span-marked rows a store may read to answer a request for limit
// well-formed beats.
//
// The well-formedness test stays on the client side: a near-miss row wears the span actor and
// method without a round-tripping path, and it must be skipped without consuming a result slot,
// which the query cannot express. The scan still has to be bounded, or a chain holding fewer beats
// than the caller asked for reads every span-marked row it has, which is the whole table for a
// chain that beats. Four times the limit plus a floor reaches past any realistic run of near-miss
// rows while keeping the read proportional to what was asked for.
func SpanScanLimit(limit int) int {
	const factor, floor = 4, 64
	if limit < 1 {
		limit = 1
	}
	if limit > (math.MaxInt-floor)/factor {
		return math.MaxInt
	}
	return limit*factor + floor
}

// NewSpanEntry builds the chain entry for one beat. It is appended like any mutation, so the
// chain commits to the beat the same way it commits to a change.
func NewSpanEntry(at time.Time, beat, count int64, cadenceS int) *Entry {
	return &Entry{
		ID: NewID(), At: at,
		Actor: SpanActor, Method: SpanMethod, Path: SpanPath(beat, count, cadenceS),
	}
}
