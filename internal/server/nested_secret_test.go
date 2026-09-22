package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestANestedSecretNeverReachesARunRead pins the scrubber on the surface every role reads most.
//
// A run carries its launch variables verbatim, and an operator who passes a secret in one has put
// it there. The receipt, the dossier, the change register, and the webhook notification all scrub
// them. This surface scrubbed only top-level string values, so a secret one level down, a secret
// inside an array, and a secret-named key holding a number each walked through untouched and came
// back in the clear to any viewer who could read the run.
func TestANestedSecretNeverReachesARunRead(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run_secret", Playbook: "site.yml", Status: run.StatusSucceeded,
		Command: "echo hi",
		ExtraVars: map[string]any{
			"nested":      map[string]any{"password": "hunter2"},
			"db_password": []any{"hunter2"},
			"deep":        map[string]any{"a": []any{map[string]any{"api_key": "sk-live-xyz"}}},
			"innocent":    "nothing to see",
		},
	}
	// A viewer, the lowest role that can read a run at all.
	ctx := actorCtx(Actor{UserID: "u_view", Role: user.RoleViewer})

	encoded, err := json.Marshal(scrubbedRun(ctx, r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	for _, secret := range []string{"hunter2", "sk-live-xyz"} {
		if strings.Contains(body, secret) {
			t.Errorf("a run read disclosed %q to a viewer:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "nothing to see") {
		t.Error("the scrubber removed a variable that held no secret; a run read must still say " +
			"what the run did")
	}

	// The stored run is untouched: a scrubber that edits the record it protects is worse than one
	// that misses, because the original is then gone.
	if got := r.ExtraVars["nested"].(map[string]any)["password"]; got != "hunter2" {
		t.Errorf("the stored run was mutated by the scrubber: nested password = %v", got)
	}
}

// TestAnAdminStillReadsTheOriginal keeps the scrub from becoming an obstruction for the one role
// that already holds every credential on the install, which is the reasoning redactInventories
// records and this test pins so a later tightening does not quietly break it.
func TestAnAdminStillReadsTheOriginal(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run_admin", Playbook: "site.yml", Status: run.StatusSucceeded,
		ExtraVars: map[string]any{"nested": map[string]any{"password": "hunter2"}},
	}
	ctx := actorCtx(Actor{UserID: "u_admin", Role: user.RoleAdmin})
	encoded, err := json.Marshal(scrubbedRun(ctx, r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "hunter2") {
		t.Error("an admin no longer reads the original variables")
	}
}

var _ = context.Background
