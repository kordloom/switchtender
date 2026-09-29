package server

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// gzipTestHandler returns a server holding enough runs that the run list is worth compressing, plus
// one terminal run whose stream ends on its own.
func gzipTestHandler(t *testing.T) http.Handler {
	t.Helper()
	store := run.NewMemStore()
	created := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for i := range 40 {
		if err := store.Save(context.Background(), &run.Run{
			ID:        "run_" + strconv.Itoa(i),
			Playbook:  "playbooks/site-with-a-long-enough-name.yml",
			Inventory: "inventories/production.ini",
			Status:    run.StatusSucceeded,
			CreatedAt: created.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
}

// getWith performs a GET carrying the given Accept-Encoding and returns the recorder.
func getWith(handler http.Handler, path, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestCompressedJSONCarriesTheSameBytes proves a JSON response is compressed when the client asks
// for it, and that what comes back out is byte for byte what an uncompressed client receives.
func TestCompressedJSONCarriesTheSameBytes(t *testing.T) {
	t.Parallel()
	handler := gzipTestHandler(t)

	plain := getWith(handler, "/v1/runs?limit=40", "")
	if plain.Code != http.StatusOK {
		t.Fatalf("plain status = %d, want 200", plain.Code)
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("a client that did not ask for gzip got Content-Encoding %q", got)
	}
	if len(plain.Body.Bytes()) < gzipMinSize {
		t.Fatalf("the fixture body is %d bytes, too small to exercise compression", plain.Body.Len())
	}

	zipped := getWith(handler, "/v1/runs?limit=40", "gzip")
	if zipped.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", zipped.Code)
	}
	if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := zipped.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to name Accept-Encoding", got)
	}
	if got := zipped.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the handler's own type", got)
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Errorf("the compressed body is %d bytes against %d uncompressed",
			zipped.Body.Len(), plain.Body.Len())
	}

	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer func() { _ = zr.Close() }()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read compressed body: %v", err)
	}
	if diff := cmp.Diff(plain.Body.String(), string(decoded)); diff != "" {
		t.Errorf("decompressed body mismatch (-plain +decompressed):\n%s", diff)
	}
}

// TestCompressLeavesSmallAndUnaskedResponsesAlone proves the middleware only ever encodes a
// response a client asked for and that is large enough to be worth encoding.
func TestCompressLeavesSmallAndUnaskedResponsesAlone(t *testing.T) {
	t.Parallel()
	handler := gzipTestHandler(t)

	tests := []struct {
		Name   string
		Path   string
		Accept string
	}{{ // Test 0: A reply under the threshold is not worth a gzip header and trailer.
		Name: "small body", Path: "/v1/runs/run_1", Accept: "gzip",
	}, { // Test 1: A client that did not offer gzip gets none.
		Name: "not offered", Path: "/v1/runs?limit=40", Accept: "deflate",
	}, { // Test 2: A client that refused gzip by weight gets none.
		Name: "refused by weight", Path: "/v1/runs?limit=40", Accept: "gzip;q=0, deflate",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			rec := getWith(handler, test.Path, test.Accept)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q, want none", got)
			}
			if !strings.HasPrefix(rec.Body.String(), "{") {
				t.Errorf("body is not the plain JSON the handler wrote: %.40q", rec.Body.String())
			}
		})
	}
}

// TestPreparedAssetsAreNotEncodedTwice proves a static asset keeps the gzip body the UI prepared at
// startup instead of being compressed again on the way out, which would leave a browser decoding
// gzip to find more gzip.
func TestPreparedAssetsAreNotEncodedTwice(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := getWith(handler, "/ui/assets/app.js", "gzip")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Values("Content-Encoding"); len(got) != 1 || got[0] != "gzip" {
		t.Fatalf("Content-Encoding = %v, want exactly one gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer func() { _ = zr.Close() }()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read compressed asset: %v", err)
	}
	// One layer of gzip is enough to reach the script itself.
	if !strings.Contains(string(decoded), "function") {
		t.Errorf("the decoded asset is not the script: %.40q", string(decoded))
	}
}

// TestStreamIsNeverCompressed proves the live stream is passed through untouched however the client
// negotiates.
//
// Compressing a stream means holding events back until there are enough of them to fill a block,
// which is the one thing a stream must not do: the browser would sit on an empty run detail page
// while the events it is waiting for sat in a buffer here.
func TestStreamIsNeverCompressed(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_done", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := getWith(handler, "/v1/runs/run_done/stream", "gzip")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("the live stream was encoded as %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(rec.Body.String(), "event: end") {
		t.Errorf("the stream body did not arrive as readable events: %.60q", rec.Body.String())
	}
}
