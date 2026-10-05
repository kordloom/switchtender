package backup

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
)

// TestCheckRefusesARegoPolicy asserts a restore refuses a backup carrying a Rego policy before
// anything is written. A Rego policy lives in the policy file, so no backup this product writes
// holds one, and a database store has nowhere to put the modules: written through, the name would
// land without the bundle, as a rule with no criteria that holds every run.
//
// The policy is decoded from JSON, the way a backup file is read, so the bundle is compiled on the
// way in exactly as it would be from a crafted file.
func TestCheckRefusesARegoPolicy(t *testing.T) {
	t.Parallel()
	prog, err := policy.CompileRego("", "", []policy.RegoModule{{
		File: "gate.rego", Source: "package switchtender\n\nhold contains \"x\" if true\n",
	}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	raw, err := json.Marshal(&payload{Policies: []*policy.Policy{{
		ID: "pol_r", Name: "rego gate", MaxDestroy: policy.DisabledMaxDestroy, Rego: prog,
	}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	stores := freshStores()
	if err := check(context.Background(), stores, &p); !errors.Is(err, ErrFormat) {
		t.Fatalf("check() error = %v, want ErrFormat", err)
	}
	pols, err := stores.Policies.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(pols) != 0 {
		t.Errorf("a refused restore wrote %d policies", len(pols))
	}
}
