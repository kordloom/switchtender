package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// hostDiff is one host's difference between two instants.
type hostDiff struct {
	// Host is the machine.
	Host string `json:"host"`
	// State is added, removed, or changed.
	State string `json:"state"`
	// Facts names the fact keys that differ, with the value at each end, present only for a
	// changed host. Values are carried because "kernel changed" is not an answer an auditor can
	// use and "5.15.0 to 6.8.0" is.
	Facts map[string]factChange `json:"facts,omitempty"`
}

// factChange is one fact's value at each end of the window.
type factChange struct {
	// From is the value at the earlier instant.
	From string `json:"from"`
	// To is the value at the later instant.
	To string `json:"to"`
}

// The states a host can be in across a window.
const (
	// diffAdded is a host observed at the later instant and not at the earlier one.
	diffAdded = "added"
	// diffRemoved is a host observed at the earlier instant and not at the later one. It means no
	// reading survives at the later end, which is not the same as the machine being gone.
	diffRemoved = "removed"
	// diffChanged is a host observed at both ends whose facts differ.
	diffChanged = "changed"
)

// estateDiffResponse is what changed across a window.
type estateDiffResponse struct {
	// From is the earlier instant.
	From time.Time `json:"from"`
	// To is the later instant.
	To time.Time `json:"to"`
	// Hosts are the differences, ordered by host. A host that did not change is absent.
	Hosts []hostDiff `json:"hosts"`
	// Unchanged counts hosts present at both ends with identical facts, so a small list of
	// differences is read as a quiet estate rather than as a query that found nothing.
	Unchanged int `json:"unchanged"`
	// Withheld counts hosts left out at either end because the run that gathered them cannot be
	// read, which includes runs deleted by retention.
	Withheld int `json:"withheld,omitempty"`
	// Horizon is the oldest retained reading, and BeforeHistory reports that the window opens
	// before it. A window that starts before the records do reports everything as added, which
	// reads as an estate that appeared out of nothing.
	Horizon time.Time `json:"horizon,omitempty"`
	// BeforeHistory reports that from predates the oldest retained reading.
	BeforeHistory bool `json:"before_history,omitempty"`
}

// estateDiffHandler answers what changed between two instants.
//
// This is the question that follows "what did the estate look like then", and the one an audit
// actually asks: not the state at a date, but everything that moved since the last review and who
// is accountable for it. Before the state history existed neither question could be answered at
// all, because each gather destroyed the reading before it.
//
// A removed host means no reading survives at the later end. That is not the same as the machine
// being gone: it may simply not have been gathered since. The word is about the record, and the
// response says so rather than implying a decommission.
func estateDiffHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: estateDiffHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		from, ok := instantParam(w, log, r, "from", time.Time{})
		if !ok {
			return
		}
		if from.IsZero() {
			respondError(w, log, http.StatusBadRequest,
				"from is required and must be an RFC 3339 instant, for example 2026-03-01T00:00:00Z")
			return
		}
		to, ok := instantParam(w, log, r, "to", time.Now())
		if !ok {
			return
		}
		if to.Before(from) {
			respondError(w, log, http.StatusBadRequest, "to is before from, so the window is empty")
			return
		}
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the estate")
			return
		}

		earlier, withheldFrom, err := readableEstate(r.Context(), store, from, keep)
		if err != nil {
			log.Error("server: estate diff: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the estate")
			return
		}
		later, withheldTo, err := readableEstate(r.Context(), store, to, keep)
		if err != nil {
			log.Error("server: estate diff: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the estate")
			return
		}

		resp := diffEstates(earlier, later)
		resp.From, resp.To = from, to
		resp.Withheld = withheldFrom + withheldTo
		if horizon, herr := store.EstateHorizon(r.Context()); herr == nil && !horizon.IsZero() {
			resp.Horizon = horizon
			resp.BeforeHistory = from.Before(horizon)
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}

// readableEstate returns the estate at an instant, filtered to what the caller may read, and how
// many rows were withheld.
func readableEstate(ctx context.Context, store run.Store, at time.Time,
	keep func(string) bool) (map[string]run.HostFacts, int, error) {
	rows, err := store.EstateAt(ctx, at)
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]run.HostFacts, len(rows))
	withheld := 0
	for _, f := range rows {
		if !keep(f.RunID) {
			withheld++
			continue
		}
		out[f.Host] = f
	}
	return out, withheld, nil
}

// diffEstates compares two estates, ordered by host.
func diffEstates(earlier, later map[string]run.HostFacts) estateDiffResponse {
	var out estateDiffResponse
	for host, now := range later {
		before, existed := earlier[host]
		if !existed {
			out.Hosts = append(out.Hosts, hostDiff{Host: host, State: diffAdded})
			continue
		}
		changes := map[string]factChange{}
		for key, to := range now.Facts {
			if from, had := before.Facts[key]; !had || from != to {
				changes[key] = factChange{From: before.Facts[key], To: to}
			}
		}
		// A fact the host used to report and no longer does is a change too. A kernel key that
		// vanishes is not the same as one that held its value, and reading only the later side
		// would call that host unchanged.
		for key, from := range before.Facts {
			if _, still := now.Facts[key]; !still {
				changes[key] = factChange{From: from, To: ""}
			}
		}
		if len(changes) == 0 {
			out.Unchanged++
			continue
		}
		out.Hosts = append(out.Hosts, hostDiff{Host: host, State: diffChanged, Facts: changes})
	}
	for host := range earlier {
		if _, still := later[host]; !still {
			out.Hosts = append(out.Hosts, hostDiff{Host: host, State: diffRemoved})
		}
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	return out
}

// instantParam reads an RFC 3339 query parameter, answering the request itself when it is malformed
// and reporting whether the caller should carry on.
func instantParam(w http.ResponseWriter, log *zap.Logger, r *http.Request,
	name string, fallback time.Time) (time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		respondError(w, log, http.StatusBadRequest,
			name+" must be an RFC 3339 instant, for example 2026-03-01T00:00:00Z")
		return time.Time{}, false
	}
	return at, true
}
