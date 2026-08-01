package audit

import (
	"errors"
	"testing"
	"time"
)

// TestVerifyExportRefusesAnAbsentChain checks that an export holding nothing, or claiming to hold
// more than it does, is not reported as intact.
//
// Verify accepts an empty slice, the head hash of an empty chain is empty, and a signature over
// that shape is perfectly valid. So deleting the trail wholesale produced a file that verified,
// printed "chain intact: 0 entries", and exited zero. The count was equally unchecked, and it is
// the number an operator reads to notice entries are missing.
func TestVerifyExportRefusesAnAbsentChain(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	entry := &Entry{
		ID: "aud_1", Seq: 1, At: at, Actor: "admin", Method: "POST", Path: "/v1/runs",
	}
	entry.Hash = EntryHash(entry)

	tests := []struct {
		Name string
		In   *Export
	}{{ // Test 0: The trail was deleted and the file still claims to be a chain.
		Name: "no entries", In: &Export{Entries: nil, Count: 0},
	}, { // Test 1: The count says more than the file holds.
		Name: "count overstates", In: &Export{Entries: []*Entry{entry}, Count: 9},
	}, { // Test 2: The count says less, which hides entries from a reader counting rows.
		Name: "count understates", In: &Export{Entries: []*Entry{entry}, Count: 0},
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if _, err := VerifyExport(test.In); err == nil {
				t.Errorf("test %d: an export with %d entries and a count of %d verified, so a "+
					"deleted or miscounted trail reads as intact",
					testNum, len(test.In.Entries), test.In.Count)
			} else if !errors.Is(err, ErrChainBroken) {
				t.Errorf("test %d: error = %v, want ErrChainBroken", testNum, err)
			}
		})
	}

	// An honest single-entry export still verifies, so the guard did not close the ordinary case.
	honest := &Export{Entries: []*Entry{entry}, Count: 1, HeadHash: entry.Hash}
	if _, err := VerifyExport(honest); err != nil {
		t.Errorf("an honest export was refused: %v", err)
	}
}
