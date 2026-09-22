package outcome_test

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAnOversizedSpecIsStillRedacted pins the artifact a stranger is handed.
//
// The canonical reduction carries a size bailout so an enormous request body is not canonicalized
// on every digest, and above that size it returned the body unchanged. That is defensible for
// digesting somebody else's upload; it is not defensible for the spec, which this package builds
// from the run's own fields and which the receipt discloses in the clear beside its digest. A spec
// over the cap was therefore published with every secret intact, in the one artifact whose entire
// purpose is being handed to an outside auditor.
//
// The size is reached without any trick: a template's variables and a launch's variables are
// merged, each arriving under its own request cap, and JSON escaping expands the result further.
func TestAnOversizedSpecIsStillRedacted(t *testing.T) {
	t.Parallel()
	// Comfortably past the 1 MiB canonical cap once marshaled, with a secret at the end so a
	// truncating implementation cannot pass by accident.
	filler := strings.Repeat("<", 1<<20)
	r := &run.Run{
		ID: "run_big", Tool: run.ToolBash,
		Command: filler + " export PGPASSWORD=hunter2",
		ExtraVars: map[string]any{
			"db_password": "hunter2",
			"nested":      map[string]any{"api_key": "sk-live-xyz"},
		},
	}

	body, err := outcome.Spec(r)
	if err != nil {
		t.Fatalf("Spec() error = %v: an oversized spec must still produce a redacted body, "+
			"since refusing it would take the receipt away from an honest run", err)
	}
	if len(body) <= 1<<20 {
		t.Fatalf("the fixture is only %d bytes, so it never crosses the cap this test exists "+
			"for", len(body))
	}
	for _, secret := range []string{"hunter2", "sk-live-xyz"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the disclosed spec carries %q in the clear: the receipt handed to an "+
				"auditor discloses the secret the redaction exists to remove", secret)
		}
	}

	// The digest a verifier recomputes has to agree with the bytes disclosed, or an honest run
	// reads as tampered, which is the failure mode the disclosure format was chosen to avoid.
	digest, err := outcome.SpecDigest(r)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	if digest == "" {
		t.Fatal("SpecDigest returned no digest for an oversized spec")
	}
}
