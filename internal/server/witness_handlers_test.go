package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/witness"
)

// TestWitnessRouteAnswersTheClientStatusUnderTheAuditScope pins GET /v1/audit/witness: an admin
// reads the witness client's Status verbatim, a viewer and an operator are refused by the audit
// read scope, and an install that is not enrolled answers 404 as the path did before the route.
func TestWitnessRouteAnswersTheClientStatusUnderTheAuditScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens, users := auth.NewMemStore(), user.NewMemStore()
	mint := func(role user.Role) string {
		u, err := user.New(string(role)+"-witness", "pw", role)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		plain, tok, err := auth.New("t-witness-" + string(role))
		if err != nil {
			t.Fatalf("New token error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("Save token error = %v", err)
		}
		return plain
	}
	viewer, operator, admin := mint(user.RoleViewer), mint(user.RoleOperator), mint(user.RoleAdmin)
	status := witnessclient.Status{Witness: "https://witness.example", WitnessKeyID: "sha256:aa",
		InstallID: "in_test", KeyID: "sha256:bb", EnrolledAt: "2026-10-21T00:00:00Z",
		LastBeat: 7, LastSeq: 41, LastCountersignedAt: "2026-10-21T00:07:00Z", Queue: 1,
		QueueCap: 1440, LastError: "witness unreachable: down",
		LastRefusal: &witnessclient.Testimony{At: "2026-10-21T00:05:00Z", Beat: 5, Seq: 30,
			Findings: []witness.RecordedFinding{}}}
	enrolled := WitnessStatusFunc(func() witnessclient.Status { return status })

	tests := []struct {
		// Witness is the witness the server is given, nil for an install that is not enrolled.
		Witness WitnessStatuser
		// Bearer is the token the request carries.
		Bearer string
		// WantCode is the status the route must answer.
		WantCode int
		// WantStatus is the body an answered request must carry, nil when it is refused.
		WantStatus *witnessclient.Status
		// WantError is a fragment a refusal's body must contain.
		WantError string
	}{{ // Test 0: An admin reads the client's status verbatim.
		Witness: enrolled, Bearer: admin, WantCode: http.StatusOK, WantStatus: &status,
	}, { // Test 1: A viewer is refused by the audit read scope.
		Witness: enrolled, Bearer: viewer, WantCode: http.StatusForbidden,
	}, { // Test 2: An operator is refused too.
		Witness: enrolled, Bearer: operator, WantCode: http.StatusForbidden,
	}, { // Test 3: An install that is not enrolled answers 404 and says how to enroll.
		Bearer: admin, WantCode: http.StatusNotFound, WantError: "witness enroll",
	}, { // Test 4: An install that is not enrolled still refuses a viewer first.
		Bearer: viewer, WantCode: http.StatusForbidden,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
				WithUsers(users), WithWitness(test.Witness)).Handler()
			req := httptest.NewRequest(http.MethodGet, "/v1/audit/witness", nil)
			req.Header.Set("Authorization", "Bearer "+test.Bearer)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Fatalf("status code mismatch (-want +got):\n%s\n%s", diff, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantError) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), test.WantError)
			}
			if test.WantStatus == nil {
				return
			}
			var got witnessclient.Status
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode the status: %v", err)
			}
			if diff := cmp.Diff(*test.WantStatus, got); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
