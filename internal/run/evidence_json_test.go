package run

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEvidenceFieldsReachTheAPI pins that the two facts a run carries for evidence, which rule held
// it and which chain entry authorized it, are serialized rather than kept server-side.
//
// Both are read by the UI and by anyone integrating against the API, and both are the kind of field
// a well-meaning change marks json:"-" while tidying, which would leave every consumer seeing a run
// with no origin and no reason for its hold, with nothing failing.
func TestEvidenceFieldsReachTheAPI(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(&Run{
		ID: "run_1", HeldByPolicy: "prod terraform destroy", AuditReceipt: "41:9f2caa",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, want := range []string{`"held_by_policy":"prod terraform destroy"`, `"audit_receipt":"41:9f2caa"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("serialized run is missing %s\ngot: %s", want, body)
		}
	}

	// Both are omitted when empty, so a run nothing held and no request created stays clean.
	bare, err := json.Marshal(&Run{ID: "run_2"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, unwanted := range []string{"held_by_policy", "audit_receipt"} {
		if strings.Contains(string(bare), unwanted) {
			t.Errorf("a run with nothing to say carries %s", unwanted)
		}
	}
}
