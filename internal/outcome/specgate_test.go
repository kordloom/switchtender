package outcome

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// snapshotRun returns a run bound to a sealed inventory snapshot and a sealed plan file, the shape
// every field below is varied from.
func snapshotRun() *run.Run {
	return &run.Run{
		ID: "run_snap", Tool: run.ToolAnsible, Playbook: "site.yml", InventoryID: "inv_1",
		Image:      "registry.example.com/runner:1@sha256:" + strings.Repeat("a", 64),
		PlanSHA256: "plan-digest",
		InventorySnapshot: &run.InventorySnapshot{
			SealedSHA256: "sealed-digest", ContentSHA256: "content-digest", Hosts: []string{"web1"},
		},
	}
}

// TestSpecBindingCoversEverySnapshotField holds each part of a run's inventory snapshot, its plan
// file, and its pinned image to the approval. The executor refuses a run whose binding moved since
// the decision, so a part of the snapshot the binding ignores is one an approved run could be
// pointed past: a host added to the list, the sealed content swapped, or a dynamic source passed off
// as a static one. The disclosed digest moves with each as well, so a receipt shows the change.
func TestSpecBindingCoversEverySnapshotField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the field.
		Name string
		// Mutate changes the run the way a substitution would.
		Mutate func(r *run.Run)
	}{{ // Test 0: Another host added to the snapshot widens the reach.
		Name: "hosts", Mutate: func(r *run.Run) {
			r.InventorySnapshot.Hosts = append(r.InventorySnapshot.Hosts, "db1")
		},
	}, { // Test 1: Other sealed content is another inventory.
		Name: "sealed content", Mutate: func(r *run.Run) { r.InventorySnapshot.SealedSHA256 = "x" },
	}, { // Test 2: Other content under the same ciphertext digest is still other content.
		Name: "content", Mutate: func(r *run.Run) { r.InventorySnapshot.ContentSHA256 = "x" },
	}, { // Test 3: A dynamic source resolves at execution, which an approver must be told.
		Name: "dynamic", Mutate: func(r *run.Run) { r.InventorySnapshot.Dynamic = true },
	}, { // Test 4: Another plan file is another change.
		Name: "plan", Mutate: func(r *run.Run) { r.PlanSHA256 = "other" },
	}, { // Test 5: Another digest of the same tag is another image.
		Name: "image digest", Mutate: func(r *run.Run) {
			r.Image = "registry.example.com/runner:1@sha256:" + strings.Repeat("b", 64)
		},
	}}
	baseBinding, err := SpecBinding(snapshotRun())
	if err != nil {
		t.Fatalf("SpecBinding() error = %v", err)
	}
	baseDigest, err := SpecDigest(snapshotRun())
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := snapshotRun()
			test.Mutate(r)
			binding, err := SpecBinding(r)
			if err != nil {
				t.Fatalf("SpecBinding() error = %v", err)
			}
			digest, err := SpecDigest(r)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			if binding == baseBinding {
				t.Errorf("changing %s left the binding unchanged, so the executor would run it", test.Name)
			}
			if digest == baseDigest {
				t.Errorf("changing %s left the disclosed digest unchanged", test.Name)
			}
		})
	}
}

// TestDisclosedSpecNeverCarriesSealedMaterial pins that the spec a receipt discloses binds the
// sealed snapshot and plan file by digest and never carries either, however the run reads in memory.
func TestDisclosedSpecNeverCarriesSealedMaterial(t *testing.T) {
	t.Parallel()
	r := snapshotRun()
	r.InventorySealed = "SEALED-INVENTORY-BLOB"
	r.PlanSealed = "SEALED-PLAN-BLOB"
	body, err := Spec(r)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	for _, blob := range []string{r.InventorySealed, r.PlanSealed} {
		if strings.Contains(string(body), blob) {
			t.Errorf("the disclosed spec carries sealed material %q: %s", blob, body)
		}
	}
	for _, want := range []string{"sealed-digest", "plan-digest", "web1", "@sha256:"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the disclosed spec = %s, want it to bind %q", body, want)
		}
	}
}
