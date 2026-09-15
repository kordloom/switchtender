package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestDoctorReportsAnInstallThatCannotSign covers the health check missing the one thing about an
// install that matters most.
//
// A shared-database install will not mint a signing key: every process has to sign as the same
// install, so a key created by one of them would be that host's alone. That is deliberate and
// documented. The consequence was not handled anywhere. Such an install serves no receipt, no signed
// bundle and no trust document, which is the entire artifact this product is sold on, while the
// interface went on offering a Download receipt button tipped "a signed receipt anyone can verify
// offline". The published Helm chart defaults to PostgreSQL and, until now, had no way to supply a
// key at all, so a chart install could never sign. And `GET /v1/doctor`, whose whole job is to say
// what is wrong with an install, returned no findings.
func TestDoctorReportsAnInstallThatCannotSign(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which install is being checked.
		Name string
		// CanSign is whether the install holds a producer identity.
		CanSign bool
		// WantFinding is whether the doctor must report it.
		WantFinding bool
	}{{ // Test 0: No identity, which is every shared-database install without a supplied key.
		Name: "cannot sign", CanSign: false, WantFinding: true,
	}, { // Test 1: An ordinary install that signs, which must stay quiet.
		Name: "can sign", CanSign: true, WantFinding: false,
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			h := doctorHandler(nil, nil, nil, nil, nil,
				func() bool { return test.CanSign }, zap.NewNop())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", test.Name, rec.Code)
			}
			var report struct {
				Findings []doctorFinding `json:"findings"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("%s: %v", test.Name, err)
			}
			var found *doctorFinding
			for i := range report.Findings {
				if report.Findings[i].ObjectID == "identity" {
					found = &report.Findings[i]
				}
			}
			if test.WantFinding && found == nil {
				t.Fatal("an install that cannot produce a receipt, a bundle or a trust document " +
					"reported a clean bill of health")
			}
			if !test.WantFinding && found != nil {
				t.Fatalf("an install that signs was told it cannot: %+v", *found)
			}
			if found == nil {
				return
			}
			if found.Severity != "broken" {
				t.Errorf("severity = %q, want broken: the product's headline artifact is absent",
					found.Severity)
			}
			// The finding has to name the remedy, or it is only a complaint.
			if !strings.Contains(found.Problem, "SWITCHTENDER_AUDIT_KEY") {
				t.Errorf("the finding does not name the variable that fixes it: %q", found.Problem)
			}
		})
	}
}
