package server

import (
	"fmt"
	"testing"
	"time"
)

// TestOneCallerCannotEvictAnothersTicket pins that minting in a loop costs the caller its own
// tickets and nobody else's.
//
// The table was bounded in total but not per caller, and a caller at the bound dropped arbitrary
// tickets so that it could still get one. Those tickets belong to everybody, in every organization,
// so a viewer, the lowest role that can read a run, could loop the mint endpoint and take stream
// access away from other people's sessions.
func TestOneCallerCannotEvictAnothersTicket(t *testing.T) {
	t.Parallel()
	s := newStreamTickets()

	victim, err := s.mint(Actor{UserID: "alice"}, "run_alice")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for i := 0; i < streamTicketMax*2; i++ {
		if _, err := s.mint(Actor{UserID: "mallory"}, "run_mallory"); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if _, ok := s.redeem(victim, "run_alice"); !ok {
		t.Fatal("a second caller's minting evicted this caller's unexpired ticket")
	}
	if held := s.byActor["user:mallory"]; held > streamTicketPerActor {
		t.Errorf("one caller holds %d tickets, want at most %d", held, streamTicketPerActor)
	}
}

// TestACallerAtItsBoundStillMints pins that hitting the per-caller bound does not refuse the
// caller, since a caller who cannot get a ticket cannot watch its own run. It gives up its oldest.
func TestACallerAtItsBoundStillMints(t *testing.T) {
	t.Parallel()
	s := newStreamTickets()
	first, err := s.mint(Actor{UserID: "bob"}, "run_1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for i := 0; i < streamTicketPerActor; i++ {
		if _, err := s.mint(Actor{UserID: "bob"}, fmt.Sprintf("run_%d", i+2)); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	newest, err := s.mint(Actor{UserID: "bob"}, "run_newest")
	if err != nil {
		t.Fatalf("mint newest: %v", err)
	}
	if _, ok := s.redeem(newest, "run_newest"); !ok {
		t.Error("a caller at its bound could not mint a usable ticket")
	}
	if _, ok := s.redeem(first, "run_1"); ok {
		t.Error("the caller's oldest ticket should have been the one given up")
	}
}

// TestTheCountTracksTheTable pins that the per-caller count cannot drift from the tickets it
// describes, since a count that only ever rose would lock a caller out of minting.
func TestTheCountTracksTheTable(t *testing.T) {
	t.Parallel()
	s := newStreamTickets()
	var values []string
	for i := 0; i < 10; i++ {
		v, err := s.mint(Actor{UserID: "carol"}, fmt.Sprintf("run_%d", i))
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		values = append(values, v)
	}
	if got := s.byActor["user:carol"]; got != 10 {
		t.Fatalf("count after minting = %d, want 10", got)
	}
	for i, v := range values {
		if _, ok := s.redeem(v, fmt.Sprintf("run_%d", i)); !ok {
			t.Fatalf("redeem %d failed", i)
		}
	}
	if got := s.byActor["user:carol"]; got != 0 {
		t.Errorf("count after redeeming every ticket = %d, want 0", got)
	}
	if len(s.live) != 0 {
		t.Errorf("live tickets = %d, want 0", len(s.live))
	}

	// Expiry is the other way a ticket leaves, and it must return the slot too.
	base := time.Now()
	s.now = func() time.Time { return base }
	if _, err := s.mint(Actor{UserID: "dave"}, "run_x"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.now = func() time.Time { return base.Add(2 * streamTicketTTL) }
	for i := 0; i < streamTicketPerActor+1; i++ {
		if _, err := s.mint(Actor{UserID: "dave"}, "run_y"); err != nil {
			t.Fatalf("mint: %v", err)
		}
	}
	if got := s.byActor["user:dave"]; got > streamTicketPerActor {
		t.Errorf("expired tickets kept counting: %d held, want at most %d", got, streamTicketPerActor)
	}
}
