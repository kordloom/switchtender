package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// streamTicketTTL is how long a ticket is good for. It only has to survive the moment between
	// asking for it and the browser opening the stream, so it is short enough that one captured from
	// a log is almost always already dead.
	streamTicketTTL = 30 * time.Second
	// streamTicketMax bounds how many live tickets are held, so a caller looping the mint endpoint
	// cannot grow the map without limit. The oldest are dropped once the bound is passed.
	streamTicketMax = 4096
	// streamTicketPerActor bounds how many one caller may hold, so filling the table is not something
	// a single caller can do. Without it the eviction below is reachable by one account: a viewer,
	// the lowest role that can read a run, could loop the mint endpoint and drop tickets belonging to
	// everyone else, in any organization. A caller at this bound evicts only its own oldest ticket,
	// which keeps the reason the eviction exists, that a caller who cannot get a ticket cannot watch
	// its own run, without letting one caller spend everybody else's.
	streamTicketPerActor = 64
)

// streamTicket is one minted permission to open one run's event stream.
type streamTicket struct {
	// actor is who asked for it, replayed onto the stream request so authorization is unchanged.
	actor Actor
	// runID is the single run this ticket opens. A ticket for one run opens no other.
	runID string
	// expires is when it stops working.
	expires time.Time
}

// streamTickets mints and redeems short-lived permissions to open one run's event stream.
//
// EventSource cannot set headers, so the stream endpoint used to accept the caller's own bearer
// token as a query parameter. The application never wrote that URL into an href and the audit chain
// records only the path, but a URL is not private: nginx, Traefik, and an ALB all log the full
// request line by default, so a thirty-day session credential ended up in access logs, log
// shippers, and whatever holds them. A ticket in the same position is worth almost nothing: it opens
// one run, it is single use, and it is dead within thirty seconds.
type streamTickets struct {
	// mu guards live.
	mu sync.Mutex
	// live holds unredeemed tickets by their secret value.
	live map[string]streamTicket
	// byActor is how many live tickets each caller holds, keyed the way the stream limiter keys one.
	byActor map[string]int
	// now reads the clock, replaced in tests.
	now func() time.Time
}

// newStreamTickets returns an empty ticket store.
func newStreamTickets() *streamTickets {
	return &streamTickets{
		live: make(map[string]streamTicket), byActor: make(map[string]int), now: time.Now,
	}
}

// ticketActorKey identifies the caller a ticket is counted against, on the same terms the live
// stream limiter counts one, so the two bounds describe the same caller.
func ticketActorKey(a Actor) string {
	switch {
	case a.UserID != "":
		return "user:" + a.UserID
	case a.Name != "":
		return "name:" + a.Name
	default:
		return "anon"
	}
}

// drop removes one ticket and keeps the per-caller count with it. Every removal goes through here,
// so the count cannot drift from the table it describes.
func (s *streamTickets) drop(value string) {
	t, ok := s.live[value]
	if !ok {
		return
	}
	delete(s.live, value)
	key := ticketActorKey(t.actor)
	if s.byActor[key] <= 1 {
		delete(s.byActor, key)
		return
	}
	s.byActor[key]--
}

// mint records a ticket for actor to open runID and returns its secret.
func (s *streamTickets) mint(actor Actor, runID string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	value := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := ticketActorKey(actor)
	if len(s.live) >= streamTicketMax || s.byActor[key] >= streamTicketPerActor {
		for k, t := range s.live {
			if now.After(t.expires) {
				s.drop(k)
			}
		}
	}
	// A caller at its own bound gives up its oldest rather than anyone else's, so looping the
	// endpoint costs the caller its own tickets and nobody else theirs.
	for s.byActor[key] >= streamTicketPerActor {
		oldest, found := "", time.Time{}
		for k, t := range s.live {
			if ticketActorKey(t.actor) != key {
				continue
			}
			if found.IsZero() || t.expires.Before(found) {
				oldest, found = k, t.expires
			}
		}
		if oldest == "" {
			break
		}
		s.drop(oldest)
	}
	// The table as a whole can still fill when many callers each hold a legitimate share, and a
	// caller who cannot get a ticket cannot watch their own run, so the last resort is unchanged.
	for k := range s.live {
		if len(s.live) < streamTicketMax {
			break
		}
		s.drop(k)
	}
	s.live[value] = streamTicket{actor: actor, runID: runID, expires: now.Add(streamTicketTTL)}
	s.byActor[key]++
	return value, nil
}

// redeem consumes a ticket for runID and returns who minted it. A ticket is good once: redeeming it
// removes it, so one captured in a log cannot be replayed even inside its lifetime.
func (s *streamTickets) redeem(value, runID string) (Actor, bool) {
	if value == "" || runID == "" {
		return Actor{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.live[value]
	if !ok {
		return Actor{}, false
	}
	s.drop(value)
	if s.now().After(t.expires) {
		return Actor{}, false
	}
	// Compared in constant time and against the run in the path, so a ticket for one run cannot be
	// presented on another.
	if subtle.ConstantTimeCompare([]byte(t.runID), []byte(runID)) != 1 {
		return Actor{}, false
	}
	return t.actor, true
}

// streamTicketHandler mints a ticket for the run named in the path, for a caller who has already
// been authorized to read that run by the ordinary header-authenticated route.
func streamTicketHandler(tickets *streamTickets, log *zap.Logger) http.HandlerFunc {
	if tickets == nil {
		panic("server: streamTicketHandler: tickets required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFrom(r.Context())
		if !ok {
			// An install running open has no actor to bind, and its stream needs no ticket either.
			respondError(w, log, http.StatusNotFound, "stream tickets are not in use")
			return
		}
		runID := r.PathValue("id")
		value, err := tickets.mint(actor, runID)
		if err != nil {
			log.Error("server: mint stream ticket: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not open a stream")
			return
		}
		respondJSON(w, log, http.StatusCreated, map[string]any{
			"ticket":     value,
			"expires_in": int(streamTicketTTL.Seconds()),
		}, wantsPretty(r))
	}
}
