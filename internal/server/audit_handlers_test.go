package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
)

// getBeats requests the beat feed without any credential and decodes the answer.
func getBeats(t *testing.T, handler http.Handler, target string) (int, []beatRecord) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var beats []beatRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &beats); err != nil {
		t.Fatalf("beats feed is not a JSON array: %v", err)
	}
	if beats == nil {
		t.Fatal("beats feed decoded to null, want an array even when empty")
	}
	return rec.Code, beats
}

// enforcedHandler returns a handler whose token gate is enforcing, holding one token with the
// given name, and the token's plain text. The tests prove which paths stay reachable without a
// credential and what the one credential can and cannot do.
func enforcedHandler(t *testing.T, audits audit.Store, tokenName string) (http.Handler, string) {
	t.Helper()
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New(tokenName)
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	if err := tokens.Save(context.Background(), tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	return New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithAudit(audits)).Handler(), plain
}

// TestAuditBeatsFeed pins the public beat feed: oldest first, the documented fields taken from the
// chain itself, a limit that keeps the newest beats, and unauthenticated access while the rest of
// the audit API stays gated.
func TestAuditBeatsFeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	// Two mutations, a beat, one mutation, a near-miss wearing the span actor with a path that does
	// not round-trip, then two back-to-back beats. The near-miss must stay out of the feed.
	for i, path := range []string{"/v1/runs", "/v1/projects"} {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	if _, err := audits.AppendSpanBeat(ctx, base.Add(time.Hour), 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(2 * time.Hour),
		Actor: "root", Method: "DELETE", Path: "/v1/projects/p1",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(150 * time.Minute),
		Actor: audit.SpanActor, Method: audit.SpanMethod, Path: "/span/9?count=0",
	}); err != nil {
		t.Fatalf("Append() of the near-miss error = %v", err)
	}
	for i := range 2 {
		if _, err := audits.AppendSpanBeat(ctx, base.Add(time.Duration(3+i)*time.Hour), 60); err != nil {
			t.Fatalf("AppendSpanBeat() error = %v", err)
		}
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	handler, _ := enforcedHandler(t, audits, "ci")

	// The rest of the audit API stays gated, which is what makes the feed's exemption meaningful.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/audit without a token = %d, want 401", rec.Code)
	}

	code, beats := getBeats(t, handler, "/v1/audit/beats")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/audit/beats without a token = %d, want 200", code)
	}
	// The feed's records must be the chain's span entries verbatim, oldest first, with each head
	// being the beat entry's own hash so a watcher can pin it. The near-miss at sequence five is
	// not among them.
	want := []beatRecord{{
		Beat: 1, At: chain[2].At.UTC().Format(time.RFC3339Nano), Seq: 3, Head: chain[2].Hash,
	}, {
		Beat: 2, At: chain[5].At.UTC().Format(time.RFC3339Nano), Seq: 6, Head: chain[5].Hash,
	}, {
		Beat: 3, At: chain[6].At.UTC().Format(time.RFC3339Nano), Seq: 7, Head: chain[6].Hash,
	}}
	if diff := cmp.Diff(want, beats); diff != "" {
		t.Errorf("beats mismatch (-want +got):\n%s", diff)
	}

	// A limit keeps the newest beats, still oldest first within the answer, so a capped watcher
	// sees the present end of the stream.
	code, capped := getBeats(t, handler, "/v1/audit/beats?limit=2")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/audit/beats?limit=2 = %d, want 200", code)
	}
	if diff := cmp.Diff(want[1:], capped); diff != "" {
		t.Errorf("capped beats mismatch (-want +got):\n%s", diff)
	}

	// A limit that does not parse or falls outside [1, 10000] is the default, not an error and not
	// an unbounded read: the feed is unauthenticated, so the limit is what bounds a stranger's ask.
	for _, bad := range []string{"abc", "0", "-5", "20000"} {
		code, got := getBeats(t, handler, "/v1/audit/beats?limit="+bad)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/audit/beats?limit=%s = %d, want 200", bad, code)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("limit=%s mismatch, want the default limit (-want +got):\n%s", bad, diff)
		}
	}
}

// TestForgedSpanBeatRefused pins the injection defense end to end. Token names are stored
// verbatim, HTTP methods are free text, and an encoded question mark decodes into the recorded
// path, so an authenticated caller can craft a request whose audit entry reads exactly like a span
// beat. The store's reservation must refuse it, the fail-closed middleware must answer 503, and
// the chain must be left untouched.
func TestForgedSpanBeatRefused(t *testing.T) {
	t.Parallel()
	audits := audit.NewMemStore()
	handler, plain := enforcedHandler(t, audits, audit.SpanActor)

	req := httptest.NewRequest(audit.SpanMethod, "/span/9%3Fcount=0%26cadence_s=60", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	// The crafted target must decode into a parseable span path, or this test guards nothing.
	if _, _, _, ok := audit.ParseSpanPath(req.URL.Path); !ok {
		t.Fatalf("crafted path decoded to %q, which does not parse as a span path", req.URL.Path)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("forged span beat request = %d, want 503 from the refused audit append", rec.Code)
	}
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("Chain() len = %d after the forged request, want 0: nothing may be appended", len(chain))
	}
}

// TestAuditBeatsFeedWithoutBeats pins that a chain holding no span entries answers an empty array,
// not null and not an error, so a watcher of a fresh install sees a well-formed feed.
func TestAuditBeatsFeedWithoutBeats(t *testing.T) {
	t.Parallel()
	audits := audit.NewMemStore()
	if err := audits.Append(context.Background(), &audit.Entry{
		ID: audit.NewID(), At: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Actor: "root", Method: "POST", Path: "/v1/runs",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	handler, _ := enforcedHandler(t, audits, "ci")
	code, beats := getBeats(t, handler, "/v1/audit/beats")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/audit/beats = %d, want 200", code)
	}
	if len(beats) != 0 {
		t.Errorf("beats = %+v, want an empty array for a chain with no span entries", beats)
	}
}
