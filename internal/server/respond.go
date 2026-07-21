package server

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/jsonutil"
)

// errorResponse is the JSON body returned for error responses.
type errorResponse struct {
	// Error is a human-readable failure message.
	Error string `json:"error"`
}

// wantsPretty reports whether the request asked for indented JSON via the pretty query parameter.
func wantsPretty(r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("pretty") {
		return false
	}
	return q.Get("pretty") != "false"
}

// respondJSON writes v as JSON with the given status code.
func respondJSON(w http.ResponseWriter, log *zap.Logger, status int, v any, pretty bool) {
	body, err := jsonutil.Marshal(v, pretty)
	if err != nil {
		log.Error("server: marshal response: " + err.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Error("server: write response: " + err.Error())
	}
}

// respondError writes an errorResponse with the given status code.
func respondError(w http.ResponseWriter, log *zap.Logger, status int, message string) {
	respondJSON(w, log, status, errorResponse{Error: message}, false)
}
