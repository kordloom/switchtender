package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// sealContract runs the store behavior approval binding depends on: a row's spec seal and result
// seal round trip through every full-row read, survive a whole-row save from a copy that never
// carried them, and are written by the narrow stamp and by every terminal write.
func sealContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("seals round trip through every full-row read", func(t *testing.T) {
		testSealsRoundTrip(t, newStore())
	})
	t.Run("seals survive a blank whole-row save", func(t *testing.T) {
		testSealsSurviveBlankSave(t, newStore())
	})
	t.Run("seals stamp with the approved spec", func(t *testing.T) {
		testSealsStampApprovedSpec(t, newStore())
	})
	t.Run("seals land with the terminal write", func(t *testing.T) {
		testSealsTerminalWrite(t, newStore())
	})
	t.Run("seal adopted is fenced", func(t *testing.T) {
		testSealAdoptedIsFenced(t, newStore())
	})
}

// sealedRow returns a run carrying both seals in the given status, unclaimed unless running.
func sealedRow(id string, status run.Status) *run.Run {
	r := &run.Run{
		ID: id, Playbook: "site.yml", Tool: "bash", Command: "echo " + id, Status: status,
		CreatedAt:  time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		SpecSeal:   "bk1:spec-" + id,
		ResultSeal: "bk1:result-" + id,
	}
	if status == run.StatusRunning {
		r.ClaimedBy = "worker-a"
	}
	return r
}

// findRun returns the run with id from runs, or nil.
func findRun(runs []*run.Run, id string) *run.Run {
	for _, r := range runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// testSealsRoundTrip pins that both seals come back from every read that returns full rows, for a
// top-level run and for a child, and that neither ever appears in a run's JSON.
func testSealsRoundTrip(t *testing.T, store run.Store) {
	ctx := context.Background()
	parent := sealedRow("run_seal_parent", run.StatusRunning)
	parent.Kind = "pipeline"
	parent.IdempotencyKey = "idem_seal_parent"
	child := sealedRow("run_seal_child", run.StatusPending)
	child.ParentID = &parent.ID
	child.StepIndex = intPtr(0)
	for _, r := range []*run.Run{parent, child} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	tests := []struct {
		// Name is the read under test.
		Name string
		// Read returns the rows the read answers.
		Read func() ([]*run.Run, error)
		// Want is the run whose seals must come back.
		Want *run.Run
	}{{ // Test 0: Get on a top-level run.
		Name: "Get parent", Want: parent, Read: func() ([]*run.Run, error) {
			r, err := store.Get(ctx, parent.ID)
			return []*run.Run{r}, err
		},
	}, { // Test 1: Get on a child.
		Name: "Get child", Want: child, Read: func() ([]*run.Run, error) {
			r, err := store.Get(ctx, child.ID)
			return []*run.Run{r}, err
		},
	}, { // Test 2: The idempotency key lookup.
		Name: "ByIdempotencyKey", Want: parent, Read: func() ([]*run.Run, error) {
			r, err := store.ByIdempotencyKey(ctx, parent.IdempotencyKey)
			return []*run.Run{r}, err
		},
	}, { // Test 3: The top-level list.
		Name: "List", Want: parent, Read: func() ([]*run.Run, error) { return store.List(ctx) },
	}, { // Test 4: The paged list.
		Name: "ListPage", Want: parent, Read: func() ([]*run.Run, error) {
			return store.ListPage(ctx, run.ListFilter{}, 10, 0)
		},
	}, { // Test 5: The step list.
		Name: "Steps", Want: child, Read: func() ([]*run.Run, error) {
			return store.Steps(ctx, parent.ID)
		},
	}, { // Test 6: The shard list, which reads the same children.
		Name: "Shards", Want: child, Read: func() ([]*run.Run, error) {
			return store.Shards(ctx, parent.ID)
		},
	}, { // Test 7: The non-terminal sweep list, parent.
		Name: "NonTerminal parent", Want: parent, Read: func() ([]*run.Run, error) {
			return store.NonTerminal(ctx)
		},
	}, { // Test 8: The non-terminal sweep list, child.
		Name: "NonTerminal child", Want: child, Read: func() ([]*run.Run, error) {
			return store.NonTerminal(ctx)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			runs, err := test.Read()
			if err != nil {
				t.Fatalf("%s error = %v", test.Name, err)
			}
			got := findRun(runs, test.Want.ID)
			if got == nil {
				t.Fatalf("%s did not return %s", test.Name, test.Want.ID)
			}
			if diff := cmp.Diff([2]string{test.Want.SpecSeal, test.Want.ResultSeal},
				[2]string{got.SpecSeal, got.ResultSeal}); diff != "" {
				t.Errorf("%s seals mismatch (-want +got):\n%s", test.Name, diff)
			}
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if strings.Contains(string(body), "bk1:") {
				t.Errorf("%s run JSON carries a seal: %s", test.Name, body)
			}
		})
	}
}

// testSealsSurviveBlankSave pins the save rule for seals: a whole-row save from a copy decoded
// from JSON, which never carries a seal, keeps the stored seals, and a save carrying a new seal
// replaces the stored one.
func testSealsSurviveBlankSave(t *testing.T, store run.Store) {
	ctx := context.Background()
	tests := []struct {
		// ID is the run saved and saved again.
		ID string
		// SpecSeal is the spec seal the second save carries.
		SpecSeal string
		// ResultSeal is the result seal the second save carries.
		ResultSeal string
		// WantSpecSeal is the spec seal read back after the second save.
		WantSpecSeal string
		// WantResultSeal is the result seal read back after the second save.
		WantResultSeal string
	}{{ // Test 0: A save with both seals blank keeps both stored seals.
		ID: "run_seal_blank", SpecSeal: "", ResultSeal: "",
		WantSpecSeal: "bk1:spec-run_seal_blank", WantResultSeal: "bk1:result-run_seal_blank",
	}, { // Test 1: A save carrying new seals replaces both.
		ID: "run_seal_new", SpecSeal: "bk1:spec-new", ResultSeal: "bk1:result-new",
		WantSpecSeal: "bk1:spec-new", WantResultSeal: "bk1:result-new",
	}, { // Test 2: Each seal follows the rule on its own.
		ID: "run_seal_mixed", SpecSeal: "bk1:spec-new", ResultSeal: "",
		WantSpecSeal: "bk1:spec-new", WantResultSeal: "bk1:result-run_seal_mixed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if err := store.Save(ctx, sealedRow(test.ID, run.StatusPending)); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			stored, err := store.Get(ctx, test.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			body, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var decoded run.Run
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			decoded.SpecSeal, decoded.ResultSeal = test.SpecSeal, test.ResultSeal
			if err := store.Save(ctx, &decoded); err != nil {
				t.Fatalf("second Save() error = %v", err)
			}
			got, err := store.Get(ctx, test.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff([2]string{test.WantSpecSeal, test.WantResultSeal},
				[2]string{got.SpecSeal, got.ResultSeal}); diff != "" {
				t.Errorf("seals mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// testSealsStampApprovedSpec pins that the approval stamp writes the spec seal beside the digest
// and the binding, leaves the result seal alone, and reports a missing run.
func testSealsStampApprovedSpec(t *testing.T, store run.Store) {
	ctx := context.Background()
	held := sealedRow("run_seal_stamp", run.StatusPendingApproval)
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.StampApprovedSpec(ctx, held.ID, "sha256:digest", "sha256:binding",
		"bk1:approved"); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	got, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := []string{"sha256:digest", "sha256:binding", "bk1:approved", held.ResultSeal}
	gotFields := []string{got.ApprovedSpecDigest, got.ApprovedSpecBinding, got.SpecSeal,
		got.ResultSeal}
	if diff := cmp.Diff(want, gotFields); diff != "" {
		t.Errorf("stamped fields mismatch (-want +got):\n%s", diff)
	}
	err = store.StampApprovedSpec(ctx, "run_seal_ghost", "sha256:digest", "sha256:binding",
		"bk1:approved")
	if !errors.Is(err, run.ErrNotFound) {
		t.Errorf("StampApprovedSpec(missing run) = %v, want run.ErrNotFound", err)
	}
}

// testSealAdoptedIsFenced pins the write adoption seals rows with. Each seal lands only on an empty
// column and a claim only over the claim adoption read, so a row another process sealed or decided
// after the read is left whole, every column the write names refusing it together. A missing row is
// run.ErrNotFound, an adoption naming nothing or a claim without the one it replaces is refused
// before anything is written, and no column but the ones written ever moves: the approved digest,
// the binding, the status, and the decision a claim names stay as they were.
//
//nolint:funlen // Test function.
func testSealAdoptedIsFenced(t *testing.T, store run.Store) {
	ctx := context.Background()
	// claimed is the claim a decision stores on a row, and adopted the same claim completed.
	const claimed = `{"id":"dec_adopt","verdict":"approved","status":"pending"}`
	const adopted = `{"id":"dec_adopt","verdict":"approved","status":"pending","claim_seal":"bk1:c"}`
	tests := []struct {
		// SpecSeal is the spec seal the row carries before the write.
		SpecSeal string
		// ResultSeal is the result seal the row carries before the write.
		ResultSeal string
		// Claimed claims the row for a decision before the write.
		Claimed bool
		// Missing writes to a row that was never saved.
		Missing bool
		// Adoption is the write.
		Adoption run.Adoption
		// WantChanged is whether the write reports it changed the row.
		WantChanged bool
		// WantWritten is whether the row reads back with the adoption's columns written.
		WantWritten bool
		// Want is the error the write returns.
		Want error
	}{{ // Test 0: A spec seal lands on an empty column.
		Adoption:    run.Adoption{SpecSeal: "bk1:spec"},
		WantChanged: true, WantWritten: true,
	}, { // Test 1: A result seal lands on an empty column.
		Adoption:    run.Adoption{ResultSeal: "bk1:result"},
		WantChanged: true, WantWritten: true,
	}, { // Test 2: Both seals land together.
		Adoption:    run.Adoption{SpecSeal: "bk1:spec", ResultSeal: "bk1:result"},
		WantChanged: true, WantWritten: true,
	}, { // Test 3: A spec seal already present refuses the write.
		SpecSeal: "bk1:other", Adoption: run.Adoption{SpecSeal: "bk1:spec"},
	}, { // Test 4: A result seal already present refuses the write.
		ResultSeal: "bk1:other", Adoption: run.Adoption{ResultSeal: "bk1:result"},
	}, { // Test 5: A result seal already present refuses the spec seal written beside it.
		ResultSeal: "bk1:other",
		Adoption:   run.Adoption{SpecSeal: "bk1:spec", ResultSeal: "bk1:result"},
	}, { // Test 6: A claim lands over the claim it was read as.
		Claimed:     true,
		Adoption:    run.Adoption{Claim: adopted, PrevClaim: claimed},
		WantChanged: true, WantWritten: true,
	}, { // Test 7: A claim that moved since it was read refuses the write.
		Claimed:  true,
		Adoption: run.Adoption{Claim: adopted, PrevClaim: `{"id":"dec_adopt"}`},
	}, { // Test 8: A row that carries no claim refuses a claim written over one.
		Adoption: run.Adoption{Claim: adopted, PrevClaim: claimed},
	}, { // Test 9: A spec seal already present refuses the claim written beside it.
		SpecSeal: "bk1:other", Claimed: true,
		Adoption: run.Adoption{SpecSeal: "bk1:spec", Claim: adopted, PrevClaim: claimed},
	}, { // Test 10: A claim that moved refuses the spec seal written beside it.
		Claimed:  true,
		Adoption: run.Adoption{SpecSeal: "bk1:spec", Claim: adopted, PrevClaim: adopted},
	}, { // Test 11: A missing row is not found.
		Missing: true, Adoption: run.Adoption{SpecSeal: "bk1:spec"}, Want: run.ErrNotFound,
	}, { // Test 12: An adoption that names nothing is refused.
		Adoption: run.Adoption{}, Want: run.ErrNoDecision,
	}, { // Test 13: A claim without the claim it replaces is refused.
		Claimed: true, Adoption: run.Adoption{Claim: adopted}, Want: run.ErrNoDecision,
	}, { // Test 14: The claim it replaces without a claim is refused.
		Claimed: true, Adoption: run.Adoption{SpecSeal: "bk1:spec", PrevClaim: claimed},
		Want: run.ErrNoDecision,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			id := fmt.Sprintf("run_adopt_%d", testNum)
			if !test.Missing {
				r := &run.Run{
					ID: id, Playbook: "site.yml", Tool: "bash", Command: "echo " + id,
					Status:             run.StatusPendingApproval,
					CreatedAt:          time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC),
					ApprovedSpecDigest: "sha256:digest", ApprovedSpecBinding: "sha256:binding",
					SpecSeal: test.SpecSeal, ResultSeal: test.ResultSeal,
				}
				if err := store.Save(ctx, r); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				if test.Claimed {
					ok, err := store.ClaimDecision(ctx, id, "dec_adopt", claimed)
					if err != nil || !ok {
						t.Fatalf("ClaimDecision() = %v, %v, want claimed", ok, err)
					}
				}
			}
			var before *run.Run
			if !test.Missing {
				var err error
				if before, err = store.Get(ctx, id); err != nil {
					t.Fatalf("Get() error = %v", err)
				}
			}
			changed, err := store.SealAdopted(ctx, id, test.Adoption)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SealAdopted() error = %v, want %v", err, test.Want)
			}
			if changed != test.WantChanged {
				t.Errorf("SealAdopted() changed = %t, want %t", changed, test.WantChanged)
			}
			if test.Missing {
				return
			}
			want := *before
			if a := test.Adoption; test.WantWritten {
				if a.SpecSeal != "" {
					want.SpecSeal = a.SpecSeal
				}
				if a.ResultSeal != "" {
					want.ResultSeal = a.ResultSeal
				}
				if a.Claim != "" {
					want.DecisionClaim = a.Claim
				}
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(&want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("row after the write mismatch (-want +got):\n%s", diff)
			}
			if got.InFlight() != test.Claimed {
				t.Errorf("row in flight = %t, want %t", got.InFlight(), test.Claimed)
			}
		})
	}
}

// testSealsTerminalWrite pins that both terminal writes that take a Finalization record its result
// seal in the same write and leave the spec seal alone.
func testSealsTerminalWrite(t *testing.T, store run.Store) {
	ctx := context.Background()
	tests := []struct {
		// Name is the terminal write under test.
		Name string
		// Run is the row the write ends.
		Run *run.Run
		// End performs the terminal write with fin.
		End func(id string, fin run.Finalization) (bool, error)
	}{{ // Test 0: The executor's finalize of a running run.
		Name: "FinalizeRunning", Run: sealedRow("run_seal_finalize", run.StatusRunning),
		End: func(id string, fin run.Finalization) (bool, error) {
			fin.Owner = "worker-a"
			return store.FinalizeRunning(ctx, id, fin)
		},
	}, { // Test 1: The settle of a held run, which is how an approval step is decided.
		Name: "SettleHeld", Run: sealedRow("run_seal_settle", run.StatusPendingApproval),
		End: func(id string, fin run.Finalization) (bool, error) {
			return store.SettleHeld(ctx, id, fin)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			if err := store.Save(ctx, test.Run); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			fin := run.Finalization{Status: run.StatusSucceeded, ResultSeal: "bk1:result-final",
				EndedAt: test.Run.CreatedAt.Add(time.Minute)}
			moved, err := test.End(test.Run.ID, fin)
			if err != nil || !moved {
				t.Fatalf("%s() = %v, %v, want moved", test.Name, moved, err)
			}
			got, err := store.Get(ctx, test.Run.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff([2]string{test.Run.SpecSeal, "bk1:result-final"},
				[2]string{got.SpecSeal, got.ResultSeal}); diff != "" {
				t.Errorf("%s seals mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}
