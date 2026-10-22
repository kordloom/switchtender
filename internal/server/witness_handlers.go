package server

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/witnessclient"
)

// WitnessStatuser reports an install's standing with the push witness it is enrolled with.
type WitnessStatuser interface {
	// Status returns the enrollment and the pushes since the process started.
	Status() witnessclient.Status
}

// WitnessStatusFunc adapts a function to a WitnessStatuser.
type WitnessStatusFunc func() witnessclient.Status

// Status calls f.
func (f WitnessStatusFunc) Status() witnessclient.Status { return f() }

// WithWitness serves the standing of the push witness the install is enrolled with at
// GET /v1/audit/witness. A nil witness is an install that is not enrolled.
func WithWitness(w WitnessStatuser) Option {
	return func(srv *Server) { srv.witness = w }
}

// witnessHandler answers the push witness's account of this install: the enrollment, the witness
// key id, the last countersigned beat, the queue, the last error, and the last refusal with its
// findings. An install that is not enrolled answers 404, the same answer the path gave before the
// route existed.
func witnessHandler(w WitnessStatuser, log *zap.Logger) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if w == nil {
			respondError(rw, log, http.StatusNotFound, "this install is not enrolled with a "+
				"witness. Run switchtender witness enroll, then restart the server")
			return
		}
		respondJSON(rw, log, http.StatusOK, w.Status(), wantsPretty(r))
	}
}
