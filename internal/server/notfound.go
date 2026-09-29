package server

import (
	"net/http"

	"go.uber.org/zap"
)

// jsonNotFound gives the mux's own refusals the shape every handler's refusal has.
//
// ServeMux writes "404 page not found" and "Method Not Allowed" as plain text. Every other refusal
// in this API is {"error":"..."} with a JSON content type, so a client that reads the error body,
// which is every client, met two responses it could not parse on the two mistakes a caller makes
// most often while learning the API: a mistyped path and a wrong verb.
//
// The status and the Allow header the mux chose are preserved. Only the body and its content type
// are replaced, and only when the mux wrote nothing itself, so a handler that legitimately answers
// 404 with its own message keeps it.
func jsonNotFound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &muxRefusal{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if !rec.replace {
			return
		}
		message := "no such endpoint: " + r.Method + " " + r.URL.Path
		if rec.status == http.StatusMethodNotAllowed {
			message = r.Method + " is not allowed on " + r.URL.Path
			if allow := w.Header().Get("Allow"); allow != "" {
				message += "; allowed: " + allow
			}
		}
		respondError(w, zap.NewNop(), rec.status, message)
	})
}

// muxRefusal watches a response for the mux's own plain-text 404 and 405, holding the header write
// back so the body can be replaced. Anything else passes through untouched.
type muxRefusal struct {
	http.ResponseWriter
	// status is the code the handler chose.
	status int
	// replace reports whether this response is one of the mux's own refusals.
	replace bool
	// wrote records that a header has already gone out, so a passthrough is never written twice.
	wrote bool
}

// WriteHeader records the status and decides whether this is a refusal the mux wrote itself.
func (m *muxRefusal) WriteHeader(status int) {
	if m.wrote {
		return
	}
	m.status = status
	// The mux writes these two with a plain text content type. A handler's own refusal is already
	// JSON and is left alone.
	if (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) &&
		m.Header().Get("Content-Type") == "text/plain; charset=utf-8" {
		m.replace = true
		return
	}
	m.wrote = true
	m.ResponseWriter.WriteHeader(status)
}

// Flush passes a streamed flush through, and writes the header first when a handler streams without
// calling WriteHeader.
//
// Without this method the wrapper silently stopped being an http.Flusher, and every live run stream
// type-asserts for one: the assertion failed, the stream handlers refused or flushed through a nil,
// and the whole streaming suite hung. A response wrapper has to keep every optional interface the
// writer it wraps had, which is the standing hazard of wrapping one at all.
func (m *muxRefusal) Flush() {
	if !m.wrote && !m.replace {
		m.wrote = true
		m.ResponseWriter.WriteHeader(http.StatusOK)
	}
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer, so http.ResponseController reaches the real one for
// deadlines and anything else this wrapper does not implement directly.
func (m *muxRefusal) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// Write drops the mux's plain-text body when this response is being replaced.
func (m *muxRefusal) Write(b []byte) (int, error) {
	if m.replace {
		return len(b), nil
	}
	if !m.wrote {
		m.wrote = true
		m.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return m.ResponseWriter.Write(b)
}
