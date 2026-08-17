package server

import (
	"errors"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// runReceiptHandler serves a signed, offline-verifiable receipt for one run.
//
// The receipt is the artifact the product's central claim rests on, and until now the only way to
// get one was a shell on the server: the run an operator was looking at could be read, exported as
// a dossier, and streamed, but not turned into the one file an auditor can check without trusting
// this install. It signs with the same identity and produces the same bytes as the command, because
// both call one builder.
//
// ?sparse=1 discloses only this run's own chain entries, each proved to belong to the log, which is
// the shape to hand outside an install that runs other people's work. ?from=<size> pairs with it to
// prove the log only appended since a size the reader already saw.
//
// A non-admin always receives the sparse shape. The contiguous one carries the chain segment recorded
// between the run's creation and its outcome, which is the trail itself, and the trail is admin-only.
// The sparse shape names the run's outcome entry and the digest it committed but does not reproduce the
// body, since a tree leaf's hash covers its claim's whole payload.
func runReceiptHandler(store run.Store, audits audit.Store, producer *audit.Identity, version string,
	authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runReceiptHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Who is asking is settled before anything about this install is described. Answering "no
		// signing identity" to a caller who may not read this run at all tells them something about the
		// install's configuration in exchange for a request that was going to be refused.
		got, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: get run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, got) {
			return
		}
		// The receipt is drawn from the same trail as the dossier and follows the same rule: admin, or
		// the actor who asked for this run, so an agent can hand somebody a proof of its own work.
		if denyUnlessAdminOrActor(w, r, log, got) {
			return
		}
		if audits == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		if producer == nil {
			respondError(w, log, http.StatusNotFound,
				"this install has no signing identity, so it cannot sign a receipt")
			return
		}
		opts := receipt.Options{Sparse: r.URL.Query().Get("sparse") != ""}
		// A non-admin receives the sparse shape whatever they asked for. The contiguous shape carries
		// the chain segment recorded between this run's creation and its outcome, which on a shared
		// install holds other organizations' entries: their actors, their methods, and their request
		// paths with the object ids in them. The trail itself is admin-only for that reason, so leaving
		// the choice to the caller let an operator who could not read a line of it take a signed slice
		// away by asking for a receipt of their own run. The sparse shape proves the same run with the
		// same outcome and discloses nothing around it.
		if !actorIsAdmin(r) {
			opts.Sparse = true
		}
		if from := r.URL.Query().Get("from"); from != "" {
			n, perr := strconv.ParseInt(from, 10, 64)
			if perr != nil || n < 1 {
				respondError(w, log, http.StatusBadRequest,
					"from must be a chain size a reader already saw, such as an anchored head")
				return
			}
			opts.From = n
		}
		res, err := receipt.Build(r.Context(), store, audits, *producer, version, got.ID, opts)
		if err != nil {
			// A run that cannot be receipted is the ordinary case for one still running or one the
			// scheduler started before fires were recorded, so it is a 409 with the reason rather
			// than a server error: nothing is broken, this run just has nothing to attest yet.
			respondError(w, log, http.StatusConflict, err.Error())
			return
		}
		// The fingerprint travels in a header so a caller scripting this has the value to pin
		// without parsing the body, and the notes travel there too rather than being lost.
		w.Header().Set("Switchtender-Key-Id", res.KeyID)
		if res.UnanchoredSparse {
			w.Header().Set("Switchtender-Receipt-Warning", "no tree anchor covers this receipt, so "+
				"nothing outside this install fixes the root it proves membership in")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="switchtender-`+got.ID+`.receipt"`)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(res.Signed); err != nil {
			log.Error("server: write receipt: " + err.Error())
		}
	}
}
