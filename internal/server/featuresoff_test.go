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

// TestPagesNameTheFeaturesTheServerRunsWithout proves the server tells the users, credentials, and
// held pages which optional stores it was started without, so the pages do not ask for them. Each
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
			for _, path := range []string{"/ui/users", "/ui/credentials", "/ui/held"} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Host = "127.0.0.1:8080"
				handler.ServeHTTP(rec, req)
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

// TestFeaturesOffNamesTheRegisterTheLicenseRefuses proves the held page is told when the license
// leaves the period change register out, so its second step links the audit trail and not a
// register that answers with a refusal. The list is made when the server is built, so each server
// here is built under the license it is read under. Deliberately not parallel: the license is
// process state.
func TestFeaturesOffNamesTheRegisterTheLicenseRefuses(t *testing.T) {
	tests := []struct {
		WantAttr  string
		Community bool
	}{{ // Test 0: A Community install names the register after the stores it runs without.
		Community: true,
		WantAttr:  ` data-features-off="tokens credential-types federation register"`,
	}, { // Test 1: A Team install does not name the register.
		Community: false,
		WantAttr:  ` data-features-off="tokens credential-types federation"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			var body string
			held := func() {
				handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}},
					zap.NewNop()).Handler()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/held", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET /ui/held status = %d, want 200", rec.Code)
				}
				body = rec.Body.String()
			}
			if test.Community {
				asCommunity(t, held)
			} else {
				held()
			}
			if !strings.Contains(body, test.WantAttr) {
				t.Errorf("GET /ui/held does not carry %s", test.WantAttr)
			}
		})
	}
}
