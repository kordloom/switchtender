package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMuxRefusalsAreJSONLikeEveryOtherRefusal covers the only two responses in this API that a
// client could not parse.
//
// ServeMux writes "404 page not found" and "Method Not Allowed" as plain text. Every other refusal
// here is {"error":"..."} with a JSON content type, so a client that reads the error body, which is
// every client, met an unparseable response on the two mistakes a caller makes most often while
// learning an API: a mistyped path and a wrong verb.
func TestMuxRefusalsAreJSONLikeEveryOtherRefusal(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/things", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// A handler that answers 404 itself, with its own message, must keep it.
	mux.HandleFunc("GET /v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		respondError(w, nil, http.StatusNotFound, "thing not found")
	})
	h := jsonNotFound(mux)

	tests := []struct {
		Name       string
		Method     string
		Path       string
		WantStatus int
		WantIn     string
	}{{ // Test 0: An unrouted path.
		Name: "unrouted path", Method: http.MethodGet, Path: "/v1/nope",
		WantStatus: http.StatusNotFound, WantIn: "no such endpoint",
	}, { // Test 1: A routed path with the wrong verb.
		Name: "wrong method", Method: http.MethodDelete, Path: "/v1/things",
		WantStatus: http.StatusMethodNotAllowed, WantIn: "not allowed",
	}, { // Test 2: A handler's own refusal is untouched.
		Name: "handler's own 404", Method: http.MethodGet, Path: "/v1/things/abc",
		WantStatus: http.StatusNotFound, WantIn: "thing not found",
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(test.Method, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d", test.Name, rec.Code, test.WantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s: content type = %q, want JSON like every other refusal", test.Name, ct)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: body is not parseable JSON: %v (%q)", test.Name, err, rec.Body.String())
			}
			if !strings.Contains(body.Error, test.WantIn) {
				t.Errorf("%s: error = %q, want it to mention %q", test.Name, body.Error, test.WantIn)
			}
		})
	}

	// A 405 keeps the Allow header the mux computed, which is what a client uses to correct itself.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/things", nil))
	if rec.Header().Get("Allow") == "" {
		t.Error("the Allow header was lost, so a 405 no longer says which methods are accepted")
	}
}

// TestMuxRefusalWrapperStaysAFlusher pins the contract that wrapping a ResponseWriter threatens.
//
// The wrapper silently stopped being an http.Flusher, and every live run stream type-asserts for
// one. The assertion failed, the stream handlers refused or flushed through a nil, and the entire
// streaming suite hung rather than failing quickly. A response wrapper has to keep every optional
// interface the writer it wraps had.
func TestMuxRefusalWrapperStaysAFlusher(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var sawFlusher bool
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hello\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	rec := httptest.NewRecorder()
	jsonNotFound(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stream", nil))

	if !sawFlusher {
		t.Fatal("the wrapped writer is not an http.Flusher, so every live stream breaks")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data: hello") {
		t.Errorf("a streamed response did not pass through: %d %q", rec.Code, rec.Body.String())
	}
}
