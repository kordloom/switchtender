package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// sealedRun returns a running run carrying a sealed inventory snapshot and a sealed plan file, with
// the records that bind them.
func sealedRun(id string) *run.Run {
	r := sampleRun(id)
	r.Status = run.StatusRunning
	r.EndedAt = nil
	r.ExitCode = nil
	r.IdempotencyKey = ""
	r.ClaimedBy = "worker-a"
	r.InventorySealed = "sealed-inventory-" + id
	r.InventorySnapshot = &run.InventorySnapshot{
		SealedSHA256: run.SealedBlobSHA256(r.InventorySealed), ContentSHA256: "content-digest",
		Hosts: []string{"web1", "web2"},
	}
	r.PlanSealed = "sealed-plan-" + id
	r.PlanSHA256 = run.SealedBlobSHA256(r.PlanSealed)
	return r
}

// testSealedMaterial pins how every backend keeps a run's sealed inventory snapshot and sealed plan
// file: both round trip and never appear in the run's JSON, only the insert that created the run
// writes them, the records binding them are written once, and every write that ends the run wipes
// them while keeping the digests.
//
// Each rule is load bearing. A whole-row save from a JSON copy carries no sealed material, so a
// backend that took the column from the save would erase what the executor still has to open. A save
// carrying a different sealed value would swap what an approval bound. And sealed material kept
// after the run ends is a secret at rest for no reason, so a save that brought a wiped value back
// would undo the wipe.
func testSealedMaterial(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := sealedRun("run_sealed_material")
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.InventorySealed != r.InventorySealed || got.PlanSealed != r.PlanSealed {
		t.Errorf("sealed material did not round trip: inventory %q plan %q", got.InventorySealed,
			got.PlanSealed)
	}
	if diff := cmp.Diff(r.InventorySnapshot, got.InventorySnapshot, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("snapshot record mismatch (-want +got):\n%s", diff)
	}
	if got.PlanSHA256 != r.PlanSHA256 {
		t.Errorf("plan digest = %q, want %q", got.PlanSHA256, r.PlanSHA256)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(data), r.InventorySealed) || strings.Contains(string(data), r.PlanSealed) {
		t.Errorf("the run's JSON carries sealed material: %s", data)
	}
	if !strings.Contains(string(data), r.PlanSHA256) || !strings.Contains(string(data), "web1") {
		t.Errorf("the run's JSON = %s, want the digests and hosts that bind it", data)
	}

	// A whole-row save from a JSON copy carries no sealed material and must not erase it, and a copy
	// whose binding records were rewritten must not move them.
	var decoded run.Run
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	decoded.InventorySnapshot.SealedSHA256 = "rewritten"
	decoded.PlanSHA256 = "rewritten"
	if err := store.Save(ctx, &decoded); err != nil {
		t.Fatalf("Save(decoded) error = %v", err)
	}
	// A save carrying different sealed material must not swap it either.
	swapped := got.Clone()
	swapped.InventorySealed = "sealed-inventory-swapped"
	swapped.PlanSealed = "sealed-plan-swapped"
	if err := store.Save(ctx, swapped); err != nil {
		t.Fatalf("Save(swapped) error = %v", err)
	}
	kept, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if kept.InventorySealed != r.InventorySealed || kept.PlanSealed != r.PlanSealed {
		t.Errorf("a later save changed the sealed material: inventory %q plan %q",
			kept.InventorySealed, kept.PlanSealed)
	}
	if kept.InventorySnapshot == nil || kept.InventorySnapshot.SealedSHA256 != r.InventorySnapshot.SealedSHA256 ||
		kept.PlanSHA256 != r.PlanSHA256 {
		t.Errorf("a later save moved the binding records: snapshot %+v plan %q",
			kept.InventorySnapshot, kept.PlanSHA256)
	}

	// The write that ends the run wipes the sealed material and records what execution learned.
	moved, err := store.FinalizeRunning(ctx, r.ID, run.Finalization{
		Status: run.StatusSucceeded, ImageDigest: "sha256:pulled", ResolvedHosts: []string{"db9"},
		EndedAt: time.Now(),
	})
	if err != nil || !moved {
		t.Fatalf("FinalizeRunning() = %v, %v, want moved", moved, err)
	}
	ended, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if ended.InventorySealed != "" || ended.PlanSealed != "" {
		t.Errorf("the run ended still carrying sealed material: inventory %q plan %q",
			ended.InventorySealed, ended.PlanSealed)
	}
	if ended.InventorySnapshot == nil || ended.PlanSHA256 != r.PlanSHA256 {
		t.Errorf("the wipe removed the binding records: snapshot %+v plan %q", ended.InventorySnapshot,
			ended.PlanSHA256)
	}
	if ended.ImageDigest != "sha256:pulled" {
		t.Errorf("image digest = %q, want the pulled one", ended.ImageDigest)
	}
	if diff := cmp.Diff([]string{"db9"}, ended.ResolvedHosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("resolved hosts mismatch (-want +got):\n%s", diff)
	}
	// A save carrying the sealed material after the wipe does not bring it back.
	if err := store.Save(ctx, got); err != nil {
		t.Fatalf("Save(stale) error = %v", err)
	}
	if after, err := store.Get(ctx, r.ID); err != nil || after.InventorySealed != "" || after.PlanSealed != "" {
		t.Errorf("a stale save restored wiped sealed material: %v", err)
	}
}

// testSealedMaterialWipes pins the other ends of a run and the sweep: settling a held run wipes its
// sealed material, WipeSealed wipes one run, and SweepSealed wipes every ended run still carrying
// some while leaving a run that is still waiting untouched.
func testSealedMaterialWipes(t *testing.T, store run.Store) {
	ctx := context.Background()
	held := sealedRun("run_sealed_held")
	held.Status = run.StatusPendingApproval
	held.ClaimedBy = ""
	waiting := sealedRun("run_sealed_waiting")
	waiting.Status = run.StatusPending
	waiting.ClaimedBy = ""
	direct := sealedRun("run_sealed_direct")
	canceled := sealedRun("run_sealed_canceled")
	canceled.Status = run.StatusCanceled
	canceled.ClaimedBy = ""
	for _, r := range []*run.Run{held, waiting, direct, canceled} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	if ok, err := store.SettleHeld(ctx, held.ID, run.Finalization{
		Status: run.StatusRejected, EndedAt: time.Now(),
	}); err != nil || !ok {
		t.Fatalf("SettleHeld() = %v, %v, want settled", ok, err)
	}
	if err := store.WipeSealed(ctx, direct.ID); err != nil {
		t.Fatalf("WipeSealed() error = %v", err)
	}
	n, err := store.SweepSealed(ctx)
	if err != nil {
		t.Fatalf("SweepSealed() error = %v", err)
	}
	if n != 1 {
		t.Errorf("SweepSealed() wiped %d runs, want 1: only the canceled run still carried some", n)
	}

	tests := []struct {
		// ID is the run checked.
		ID string
		// WantSealed reports whether the run must still carry its sealed material.
		WantSealed bool
	}{{ // Test 0: A settled held run is wiped by the settle.
		ID: held.ID, WantSealed: false,
	}, { // Test 1: A run wiped directly is wiped.
		ID: direct.ID, WantSealed: false,
	}, { // Test 2: An ended run nothing wiped is wiped by the sweep.
		ID: canceled.ID, WantSealed: false,
	}, { // Test 3: A run still waiting keeps what it will execute.
		ID: waiting.ID, WantSealed: true,
	}}
	for _, test := range tests {
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", test.ID, err)
		}
		has := got.InventorySealed != "" && got.PlanSealed != ""
		if has != test.WantSealed {
			t.Errorf("%s carries sealed material = %v, want %v", test.ID, has, test.WantSealed)
		}
		if got.InventorySnapshot == nil || got.PlanSHA256 == "" {
			t.Errorf("%s lost the records that bind its sealed material", test.ID)
		}
	}
}
