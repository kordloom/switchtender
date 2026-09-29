package server

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestOneCallerCannotEvictAnothersTicket pins that minting in a loop costs the caller its own
// tickets and nobody else's. The table is bounded per caller, and a caller at the bound gives up
// its own oldest rather than anyone's: those tickets belong to everybody, in every organization,
// so without the per-caller bound a viewer could loop the mint endpoint and take stream access
// from other people's sessions. Asserted through the public surface, because the eviction now
// lives in the store rather than in a field this test used to reach.
func TestOneCallerCannotEvictAnothersTicket(t *testing.T) {
	t.Parallel()
	s := newStreamTickets(run.NewMemStore())

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
}

// TestACallerAtItsBoundStillMints pins that hitting the per-caller bound does not refuse the
// caller, since a caller who cannot get a ticket cannot watch its own run. It gives up its oldest.
func TestACallerAtItsBoundStillMints(t *testing.T) {
	t.Parallel()
	s := newStreamTickets(run.NewMemStore())
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
