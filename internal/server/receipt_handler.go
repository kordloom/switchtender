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
//
// The whole chain is held before anything is signed, and a chain that does not verify is refused
// with the same body and the same coordinates the bundle download uses. The contiguous shape only
// ever showed its builder the segment between the run's creation and its outcome, so a break
// outside that segment was never looked at and the endpoint answered 200 with a signed receipt
// while GET /v1/audit/verify was reporting the chain broken and GET /v1/audit/bundle was refusing
// to publish it. The receipt is handed to a third party who trusts nothing this install says, so a
// caveat that does not travel inside the signed bytes is not a caveat: served that way, the offline
// verifier reads the file and prints that nothing has been altered, because for the disclosed
// entries that is true. Refusing is the only answer available here that a relying party cannot be
// shown without. The refusal names the break, so the operator loses nothing forensic: the entries,
// the position, and GET /v1/audit/verify are all still there.
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
		// The chain is held before the run's own receiptability is considered. A run still executing
		// is the ordinary reason this endpoint says no, but on an install whose chain does not verify
		// that answer would describe one run while the install had a finding about all of them, and
		// the caller would have to ask a second endpoint to learn it.
		verdict, refused := refuseBrokenChain(w, r, audits, producer, log)
		if refused {
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
			// A refusal from the builder is a statement about the chain, not about this run, so it
			// carries the chain's reason code and the builder's own words rather than arriving as an
			// error string with a sentinel prefix naming the operation. The walk above catches the
			// ordinary tamper; this catches what it does not cover, such as an entry recorded at
			// nanosecond precision or a span beat that does not advance.
			if errors.Is(err, audit.ErrExport) {
				log.Error("server: run receipt refused: " + err.Error())
				respondJSON(w, log, http.StatusConflict, bundleRefusal{
					Error: exportRefusal(err), Reason: reasonChainUnbundlable, Count: verdict.Count,
				}, wantsPretty(r))
				return
			}
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

// refuseBrokenChain holds the whole stored chain against its links and its anchors, and answers the
// request itself when either check fails. It returns what the walk found and whether it answered,
// so a later refusal drawn from the same chain reports the same entry count rather than none.
//
// It exists because a receipt is the artifact this install hands somebody who has decided to trust
// nothing it says. Signing one while the install's own check says its chain does not hold makes the
// product an accessory to a false assurance: the disclosed entries really are intact and the proof
// really does hold for them, so the offline verifier reads the file and reports that nothing has
// been altered, with no way to know what the install knew when it signed. The caveat cannot be
// attached to the artifact from here, since the receipt's bytes are signed by the builder and
// re-marshaling them would break the signature, and a caveat carried in a response header stops at
// the first save. So the install declines to sign at all, and says where the break is.
func refuseBrokenChain(w http.ResponseWriter, r *http.Request, audits audit.Store,
	producer *audit.Identity, log *zap.Logger) (chainVerdict, bool) {
	// This is a second pass over the chain on a sound install, since the builder walks it again to
	// collect what the receipt carries. That is the price of the check living here rather than inside
	// the builder, and both passes stream one entry at a time.
	recorded, aerr := anchorsFor(r.Context(), audits)
	if aerr != nil {
		log.Error("server: read anchors: " + aerr.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read the anchors")
		return chainVerdict{}, true
	}
	anchorScan := audit.NewAnchorScanner(recorded, producer.InstallID)
	verdict, err := walkChain(r.Context(), audits, anchorScan)
	if err != nil {
		log.Error("server: chain audit entries: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
		return chainVerdict{}, true
	}
	if !verdict.OK {
		log.Error("server: run receipt refused: the stored audit chain does not verify",
			zap.String("run", r.PathValue("id")), zap.Int("broke_at", verdict.BrokeAt),
			zap.Int64("broke_seq", verdict.BrokeSeq), zap.Int("entries", verdict.Count))
		respondJSON(w, log, http.StatusConflict, bundleRefusal{
			Error:  chainBreakMessage("a receipt", verdict.BrokeAt, verdict.Count, verdict.BrokeSeq),
			Reason: reasonChainBreak, BrokeAt: verdict.BrokeAt, BrokeSeq: verdict.BrokeSeq,
			Count: verdict.Count,
		}, wantsPretty(r))
		return verdict, true
	}
	problems, unsatisfied := unsatisfiedAnchors(anchorScan, len(recorded))
	if !unsatisfied {
		return verdict, false
	}
	log.Error("server: run receipt refused: the chain does not satisfy every anchor recorded over it",
		zap.String("run", r.PathValue("id")), zap.Strings("anchor_problems", problems))
	respondJSON(w, log, http.StatusConflict, bundleRefusal{
		Error: "the chain does not satisfy every anchor recorded over it, so a receipt drawn from " +
			"it must not be published as one that does. This is a problem detected in the stored " +
			"chain, not a fault in this server",
		Reason: reasonAnchorUnsatisfied, Count: verdict.Count, AnchorProblems: problems,
	}, wantsPretty(r))
	return verdict, true
}
