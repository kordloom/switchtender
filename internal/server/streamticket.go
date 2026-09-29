package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

const (
	// streamTicketTTL is how long a ticket is good for. It only has to survive the moment between
	// asking for it and the browser opening the stream, so it is short enough that one captured
	// from a log is almost always already dead.
	streamTicketTTL = 30 * time.Second
	// streamTicketMax bounds how many live tickets are held, so a caller looping the mint endpoint
	// cannot grow the table without limit.
	streamTicketMax = 4096
	// streamTicketPerActor bounds how many one caller may hold, so filling the table is not
	// something a single caller can do. A caller at this bound gives up its own oldest ticket,
	// never anyone else's, which keeps the reason the bound exists (a caller who cannot get a
	// ticket cannot watch its own run) without letting one caller spend everybody else's.
	streamTicketPerActor = 64
)

// streamTickets mints and redeems short-lived permissions to open one run's event stream, backed
// by the shared store so a ticket minted on one replica is redeemable on any other.
//
// EventSource cannot set headers, so the stream endpoint takes a ticket in the query instead of a
// bearer token. A URL is not private: nginx, Traefik, and an ALB all log the full request line, so
// a long-lived session credential in that position ends up in access logs and everything
// downstream of them. A ticket in the same position is worth almost nothing: it opens one run, it
// is single use across every replica because redemption deletes the row, and it dies within
// thirty seconds. The store holds only the ticket's hash, so a leaked table is a list of spent and
// spendable hashes rather than credentials.
type streamTickets struct {
	// store is the shared backing the tickets live in.
	store run.Store
	// now reads the clock, replaced in tests.
	now func() time.Time
}

// newStreamTickets returns a ticket service backed by store.
func newStreamTickets(store run.Store) *streamTickets {
	return &streamTickets{store: store, now: time.Now}
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

// hashTicket is the row key: the secret never lands in the store, only its digest.
func hashTicket(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// mint records a ticket for actor to open runID and returns its secret.
func (s *streamTickets) mint(actor Actor, runID string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	value := hex.EncodeToString(raw)
	payload, err := json.Marshal(actor)
	if err != nil {
		return "", err
	}
	t := run.StreamTicket{
		SecretHash: hashTicket(value), RunID: runID, ActorKey: ticketActorKey(actor),
		Actor: payload, ExpiresAt: s.now().Add(streamTicketTTL),
	}
	if err := s.store.SaveStreamTicket(context.Background(), t,
		streamTicketPerActor, streamTicketMax); err != nil {
		return "", err
	}
	return value, nil
}

// redeem consumes a ticket for runID and returns who minted it. A ticket is good once: the store
// deletes it as it is read, so one captured in a log cannot be replayed on any replica even inside
// its lifetime.
func (s *streamTickets) redeem(value, runID string) (Actor, bool) {
	if value == "" || runID == "" {
		return Actor{}, false
	}
	payload, ok, err := s.store.RedeemStreamTicket(context.Background(),
		hashTicket(value), runID, s.now())
	if err != nil || !ok {
		return Actor{}, false
	}
	var actor Actor
	// decodeForeign, not strict: during a rolling upgrade the replica that minted this ticket and
	// the one redeeming it can be different versions, so the redeemer must tolerate a field a newer
	// minter added rather than reject a live session's stream for thirty seconds mid-deploy.
	if err := decodeForeign(payload, &actor); err != nil {
		return Actor{}, false
	}
	return actor, true
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
