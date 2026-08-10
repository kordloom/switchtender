package audit

import (
	"testing"
	"time"
)

// TestVerifyDoesNotPanicOnAHostileBundle checks that a chain carrying a null entry is refused
// rather than crashing the verifier.
//
// These slices are decoded from a document handed over by whoever is being audited, so verifying
// one is the case that must never answer with a stack trace.
func TestVerifyDoesNotPanicOnAHostileBundle(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	good := &Entry{ID: "aud_1", Seq: 1, At: at, Actor: "admin", Method: "POST", Path: "/v1/runs"}
	good.Hash = EntryHash(good)

	tests := [][]*Entry{
		{nil},
		{nil, good},
		{good, nil},
		{good, nil, nil},
	}
	for testNum, entries := range tests {
		if ok, _ := Verify(entries); ok {
			t.Errorf("test %d: a chain containing a null entry verified", testNum)
		}
		if ok, _ := VerifyRange(entries); ok {
			t.Errorf("test %d: VerifyRange accepted a null entry", testNum)
		}
	}
}
