package server

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/user"
)

// TestStreamTicketIsNarrowShortLivedAndSingleUse covers what replaced the session token in the URL.
//
// EventSource cannot set a header, so the stream endpoint took the caller's own bearer token as a
// query parameter. The application never wrote that URL into an href and the audit chain records
// only the path, but a URL is not private once it leaves the browser: nginx, Traefik, and an ALB all
// log the full request line by default, so a thirty-day session credential ended up in access logs,
// log shippers, and whatever holds them. A ticket in the same position has to be worth almost
// nothing, and that is what these four properties are.
func TestStreamTicketIsNarrowShortLivedAndSingleUse(t *testing.T) {
	t.Parallel()
	actor := Actor{UserID: "user_1", Name: "jane", Role: user.RoleOperator}

	t.Run("it opens the run it was minted for", func(t *testing.T) {
		t.Parallel()
		s := newStreamTickets()
		v, err := s.mint(actor, "run_a")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		got, ok := s.redeem(v, "run_a")
		if !ok {
			t.Fatal("a freshly minted ticket did not open its own run")
		}
		if got.UserID != actor.UserID {
			t.Errorf("actor = %+v, want the one that minted it", got)
		}
	})

	t.Run("it opens no other run", func(t *testing.T) {
		t.Parallel()
		s := newStreamTickets()
		v, err := s.mint(actor, "run_a")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, ok := s.redeem(v, "run_b"); ok {
			t.Error("a ticket for one run opened another")
		}
	})

	t.Run("it works once", func(t *testing.T) {
		t.Parallel()
		s := newStreamTickets()
		v, err := s.mint(actor, "run_a")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, ok := s.redeem(v, "run_a"); !ok {
			t.Fatal("the first redemption failed")
		}
		if _, ok := s.redeem(v, "run_a"); ok {
			t.Error("a ticket captured from a log could be replayed")
		}
	})

	t.Run("it expires", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
		s := newStreamTickets()
		s.now = func() time.Time { return now }
		v, err := s.mint(actor, "run_a")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		now = now.Add(streamTicketTTL + time.Second)
		if _, ok := s.redeem(v, "run_a"); ok {
			t.Error("an expired ticket still opened the stream")
		}
	})

	t.Run("nothing opens a stream by guessing", func(t *testing.T) {
		t.Parallel()
		s := newStreamTickets()
		for _, guess := range []string{"", "deadbeef", "0000000000000000"} {
			if _, ok := s.redeem(guess, "run_a"); ok {
				t.Errorf("the guess %q opened a stream", guess)
			}
		}
	})
}
