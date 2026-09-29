package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAuthDoesNotReopenWhenTheLastTokenIsPruned proves an install that has authenticated once never
// returns to open mode.
//
// Open mode exists so a fresh install is usable before anything is set up, and enforcement was keyed
// on the live token count. Session tokens carry a thirty day lifetime and this gate deletes an
// expired token the moment it meets one, so on a browser or single sign-on install, whose only
// tokens are sessions, the count reached zero thirty days after the last sign-in and the entire API
// answered anonymous callers with admin authority. Revoking the last API token did it at once.
// Nothing was logged, because from the server's point of view it had simply never been configured.
func TestAuthDoesNotReopenWhenTheLastTokenIsPruned(t *testing.T) {
	ctx := context.Background()
	tokens := auth.NewMemStore()

	// One token, already expired, exactly what a browser-only install has 30 days after the last
	// sign-in.
	expired := time.Now().Add(-time.Hour)
	secret, tok, err := auth.New("session-abc")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.ExpiresAt = &expired
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save: %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens)).Handler()
	probe := func(bearer string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := probe(""); code != http.StatusUnauthorized {
		t.Fatalf("with a token present, anonymous = %d, want 401", code)
	}
	// Present the expired token. The gate refuses it and deletes it.
	if code := probe(secret); code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", code)
	}
	// Let the async delete land and the enforcement cache lapse.
	for range 100 {
		if n, _ := tokens.Count(ctx); n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := tokens.Count(ctx)
	t.Logf("tokens remaining after the expired one was presented: %d", n)
	time.Sleep(enforcementCacheTTL + 50*time.Millisecond)

	code := probe("")
	t.Logf("anonymous request after the last token was pruned: HTTP %d", code)
	if code != http.StatusUnauthorized {
		t.Errorf("anonymous request after the last token was pruned = %d, want 401: the API "+
			"reopened to unauthenticated callers with admin authority", code)
	}
}
