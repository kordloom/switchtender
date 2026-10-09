package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPagesNameTheFeaturesTheServerRunsWithout proves the server tells the users and credentials
// pages which optional stores it was started without, so the pages do not ask for them. Each
// answers 404 when off, which the page reads quietly as not enabled, but the browser logs every 404
// it sees.
func TestPagesNameTheFeaturesTheServerRunsWithout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantAttr string
		Opts     []Option
	}{{ // Test 0: A server without tokens, credential types, or federation names all three.
		WantAttr: ` data-features-off="tokens credential-types federation"`,
	}, { // Test 1: Tokens and credential types wired leave only federation off.
		Opts: []Option{WithTokens(auth.NewMemStore()),
			WithCredentialTypes(credential.NewMemTypeStore())},
		WantAttr: ` data-features-off="federation"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}},
				zap.NewNop(), test.Opts...).Handler()
			for _, path := range []string{"/ui/users", "/ui/credentials"} {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
				}
				if !strings.Contains(rec.Body.String(), test.WantAttr) {
					t.Errorf("GET %s does not carry %s", path, test.WantAttr)
				}
			}
		})
	}
}
