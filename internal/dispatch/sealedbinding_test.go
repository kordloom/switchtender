package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestASealedAnswerIsOpenedOnlyIfItIsTheOneTheRunWasCreatedWith pins the executor's check of each
// sealed value against the digest the run was created with.
//
// The digests are in the run's spec, so an approval binds them, and the executor's spec check proves
// only that they did not move. The ciphertext beside them is a separate column. Swapped for another
// valid sealed answer under the same name, it opened cleanly and reached the tool while every record
// of the approval still matched, so the check has to be made on the value that is opened.
func TestASealedAnswerIsOpenedOnlyIfItIsTheOneTheRunWasCreatedWith(t *testing.T) {
	t.Parallel()
	sealer := secretVarsSealer()
	approved, err := sealer.Seal("the-approved-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	other, err := sealer.Seal("another-runs-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	bound := run.SealedDigestsOf(map[string]string{"token": approved})
	tests := []struct {
		// Run is the run handed to the executor.
		Run *run.Run
		// WantOpened is the answer the tool would receive.
		WantOpened map[string]string
		// WantReason is a fragment the refusal must contain.
		WantReason string
		// Want is the error, nil when the answer opens.
		Want error
	}{{ // Test 0: The sealed answer is the one the run was created with.
		Run: &run.Run{SealedNames: []string{"token"},
			SealedVars: map[string]string{"token": approved}, SealedDigests: bound},
		WantOpened: map[string]string{"token": "the-approved-answer"},
	}, { // Test 1: Another sealed answer under the same name, its digest left as it was.
		Run: &run.Run{SealedNames: []string{"token"},
			SealedVars: map[string]string{"token": other}, SealedDigests: bound},
		WantReason: "not the one this run was created with", Want: ErrSecretAnswer,
	}, { // Test 2: A sealed answer the run gained that nothing bound.
		Run: &run.Run{SealedNames: []string{"extra", "token"},
			SealedVars:    map[string]string{"token": approved, "extra": other},
			SealedDigests: bound},
		WantReason: `"extra"`, Want: ErrSecretAnswer,
	}, { // Test 3: A bound answer whose sealed value is gone is missing, not optional.
		Run: &run.Run{SealedNames: []string{"other"},
			SealedVars:    map[string]string{"other": other},
			SealedDigests: append(run.SealedDigestsOf(map[string]string{"other": other}), bound...)},
		WantReason: `the answer to "token" did not reach this executor`, Want: ErrSecretAnswer,
	}, { // Test 4: A run created before digests existed has nothing to hold its answer to.
		Run:        &run.Run{SealedNames: []string{"token"}, SealedVars: map[string]string{"token": other}},
		WantOpened: map[string]string{"token": "another-runs-answer"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := &Dispatcher{sealer: sealer}
			got, err := d.openSecretVars(test.Run)
			if !errors.Is(err, test.Want) {
				t.Fatalf("openSecretVars() error = %v, want %v", err, test.Want)
			}
			if test.WantReason != "" && !strings.Contains(fmt.Sprint(err), test.WantReason) {
				t.Errorf("openSecretVars() error = %v, want it to contain %q", err, test.WantReason)
			}
			if diff := cmp.Diff(test.WantOpened, got); diff != "" {
				t.Errorf("opened answers (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnApprovedRunExecutesOnlyTheSealedAnswerItWasApprovedWith drives the whole guarantee through
// a real dispatcher: a run approved with one sealed answer, whose row is then changed underneath the
// approval, never hands the tool anything but the answer the approver released.
//
// There are two ways to change it. Swapping the ciphertext alone leaves the digests in the spec
// unchanged, so the approval's binding still matches and the per-value check is what refuses it.
// Swapping the ciphertext and rewriting the digest to match moves the spec, so the binding the
// approval stamped refuses it before anything is opened. The untouched run is the control: it runs.
func TestAnApprovedRunExecutesOnlyTheSealedAnswerItWasApprovedWith(t *testing.T) {
	t.Parallel()
	sealer := secretVarsSealer()
	approved, err := sealer.Seal("the-approved-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	swapped, err := sealer.Seal("a-swapped-in-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	tests := []struct {
		// Tamper changes the approved run's row before it executes, nil to leave it alone.
		Tamper func(r *run.Run)
		// WantStatus is how the run ends.
		WantStatus run.Status
		// WantReason is a fragment of the failure, empty for a run that succeeds.
		WantReason string
		// WantToken is what the tool receives, empty when it must never run.
		WantToken string
	}{{ // Test 0: The control: the approved run executes with the approved answer.
		WantStatus: run.StatusSucceeded, WantToken: "the-approved-answer",
	}, { // Test 1: The ciphertext alone is swapped, so the binding matches and the value does not.
		Tamper: func(r *run.Run) {
			r.SealedVars = map[string]string{"token": swapped}
		},
		WantStatus: run.StatusFailed, WantReason: "not the one this run was created with",
	}, { // Test 2: The ciphertext and its digest are both rewritten, which moves the spec itself.
		Tamper: func(r *run.Run) {
			run.WithSealedVars(map[string]string{"token": swapped})(r)
		},
		WantStatus: run.StatusFailed, WantReason: "the spec changed after it was approved",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{
				ID: run.NewID(), Tool: run.ToolBash, Command: "rotate", Status: run.StatusPending,
				CreatedAt: time.Now(),
			}
			run.WithSealedVars(map[string]string{"token": approved})(r)
			// What an approval stamps: the digest and the binding of the spec as it was released.
			digest, err := outcome.SpecDigest(r)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			binding, err := outcome.SpecBinding(r)
			if err != nil {
				t.Fatalf("SpecBinding() error = %v", err)
			}
			r.ApprovedSpecDigest, r.ApprovedSpecBinding = digest, binding
			if test.Tamper != nil {
				test.Tamper(r)
			}
			store := run.NewMemStore()
			if err := store.Save(context.Background(), r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			runner := &echoSecretRunner{}
			d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), sealer))
			defer d.Close()

			got := waitTerminal(t, store, r.ID)
			if got.Status != test.WantStatus {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Error, test.WantStatus)
			}
			if test.WantReason != "" && !strings.Contains(got.Error, test.WantReason) {
				t.Errorf("error = %q, want it to contain %q", got.Error, test.WantReason)
			}
			specs := runner.seen()
			if test.WantToken == "" {
				if len(specs) != 0 {
					t.Errorf("the tool ran with %v under an approval of a different answer", specs)
				}
				return
			}
			if len(specs) != 1 || specs[0]["token"] != test.WantToken {
				t.Errorf("the tool received %v, want token %q", specs, test.WantToken)
			}
		})
	}
}
