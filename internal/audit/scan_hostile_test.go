package audit

import (
	"fmt"
	"testing"
	"time"
)

// TestATamperedEntryBreaksTheChainRatherThanCrashing pins that the verifying walk reports an entry
// it cannot canonicalize, instead of panicking on it.
//
// Recomputing a link goes through JCS, which refuses two things a tampered row can hold: an integer
// above 2^53, and text that is not valid UTF-8. The producing side escapes its text and mints its
// own sequences so neither is reachable there, but the scanner reads rows back, and an audit log
// somebody has interfered with is the exact input it exists to judge. Crashing on it turns tamper
// detection into a denial of service triggered by the tampering.
func TestATamperedEntryBreaksTheChainRatherThanCrashing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Name  string
		Entry *Entry
	}{{ // Test 0: A sequence JCS will not serialize.
		Name:  "sequence above 2^53",
		Entry: &Entry{Seq: 1 << 60, At: at, Actor: "a", Method: "GET", Path: "/x"},
	}, { // Test 1: Invalid UTF-8 written straight into a stored row. SQLite stores arbitrary bytes,
		// so this survives a round trip on one of the two supported backends.
		Name:  "invalid utf-8 actor",
		Entry: &Entry{Seq: 1, At: at, Actor: "\xff\xfe", Method: "GET", Path: "/x"},
	}, { // Test 2: The same in the path, which is the field a request can steer.
		Name:  "invalid utf-8 path",
		Entry: &Entry{Seq: 1, At: at, Actor: "a", Method: "GET", Path: "/v1/x/\xff"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			// A hash is present, so the entry is refused for failing to recompute rather than for
			// being obviously empty.
			e := *test.Entry
			e.Hash = "0000000000000000000000000000000000000000000000000000000000000000"

			v := NewChainScanner(false)
			v.Feed(&e)
			ok, brokeAt, count := v.Result()
			if ok {
				t.Fatalf("an entry that cannot be canonicalized was accepted as sound")
			}
			if brokeAt != 1 || count != 1 {
				t.Errorf("brokeAt = %d, count = %d, want 1 and 1", brokeAt, count)
			}
		})
	}
}

// TestCheckedEntryHashAgreesWithEntryHash pins that the checked recompute the scanner uses returns
// the same link as the producing one for every entry the producer can actually build, so routing
// verification through it cannot change what verifies.
func TestCheckedEntryHashAgreesWithEntryHash(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	entries := []*Entry{
		{Seq: 1, At: at, Actor: "release-token", Method: "POST", Path: "/api/runs"},
		{Seq: 2, At: at, Actor: "a", Method: "GET", Path: "/x", PrevHash: "beef",
			ActorType: "token", OnBehalfOf: "ops@example.com", ContentDigest: "sha256:dead"},
		{Seq: 3, At: at, Actor: "a", Method: "GET", Path: "/x&y<z>", InstallID: "in_abc"},
	}
	for testNum, e := range entries {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := checkedEntryHash(e)
			if err != nil {
				t.Fatalf("checkedEntryHash error = %v", err)
			}
			if want := EntryHash(e); got != want {
				t.Errorf("checked hash %s, producing hash %s", got, want)
			}
		})
	}
}

// TestAWindowMustNameTheEntryBeforeIt pins that a range opening above sequence one is refused when
// it carries no previous link, even though every link in it recomputes.
//
// A window is the partial export: it starts mid-chain, so its first claim is the only thing tying
// what it shows to the history it was cut from. Drop the previous link and the window still
// verifies internally while claiming nothing about what came before, which is how a range with the
// inconvenient entries removed would present itself. The rule is symmetric, so sequence one is
// refused for carrying a previous link it cannot have.
func TestAWindowMustNameTheEntryBeforeIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Name  string
		Entry *Entry
	}{{ // Test 0: A window opening at sequence two with nothing named before it.
		Name:  "window without a previous link",
		Entry: &Entry{Seq: 2, At: at, Actor: "a", Method: "GET", Path: "/x"},
	}, { // Test 1: Genesis cannot name a predecessor, since nothing precedes it.
		Name: "genesis carrying a previous link",
		Entry: &Entry{Seq: 1, At: at, Actor: "a", Method: "GET", Path: "/x",
			PrevHash: "1111111111111111111111111111111111111111111111111111111111111111"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			e := *test.Entry
			// Hashed as it stands, so the entry is internally sound and can only be refused for
			// the structural claim it makes.
			e.Hash = EntryHash(&e)

			v := NewChainScanner(false)
			v.Feed(&e)
			if ok, _, _ := v.Result(); ok {
				t.Fatalf("a window that does not tie to the chain before it was accepted")
			}
			// And the export path refuses to build from it at all, rather than handing an auditor
			// a bundle that will be rejected once it has already been sent.
			if _, err := BuildBundle([]*Entry{&e}, Identity{InstallID: "in_test"}, "test",
				at); err == nil {
				t.Errorf("BuildBundle accepted a range that does not verify")
			}
		})
	}
}
