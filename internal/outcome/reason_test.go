package outcome_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// zeroHostReason is the failure text a zero-host Ansible run carries, copied from the dispatcher so
// this package's tests pin the real string rather than a paraphrase of it. The dispatcher owns the
// wording; what is pinned here is that a reason of this shape reaches the record intact.
const zeroHostReason = "no host was touched: the playbook recap named no host, so nothing ran. " +
	"The inventory may be empty or missing, the play's host pattern may match none of it, or a " +
	"tag filter may have left no task."

// TestRecordCarriesTheRunsFailureReason is the defect this field exists for. A zero-host run
// commits status failed beside exit code 0, and before the reason was recorded the chain held that
// pair and nothing else: an auditor read a run that failed with a clean exit code and no reason,
// is a record of a verdict with the grounds withheld.
//
// The key set is asserted whole rather than only the reason, because that is the compatibility
// claim. A run that carried no failure text must emit exactly the keys it always did, so its
// outcome digest is the digest it was committed under and a receipt rebuilt from the store matches
// what the chain holds. The reason only ever adds a key, and only to a run that has one.
func TestRecordCarriesTheRunsFailureReason(t *testing.T) {
	t.Parallel()
	// The field set an outcome body carried before the reason was recorded, for a run holding
	// nothing optional. Every case below is this list, plus "error" when the run failed with text.
	base := []string{"exit_code", "log_sha256", "playbook", "run_id", "spec_digest", "status", "tool"}
	tests := []struct {
		Name      string
		Status    run.Status
		Error     string
		WantError string
		WantKeys  []string
		ExitCode  int
	}{{ // Test 0: The zero-host run. Failed, exit zero, and now the chain says why.
		Name: "zero host", Status: run.StatusFailed, ExitCode: 0, Error: zeroHostReason,
		WantError: zeroHostReason, WantKeys: append(append([]string{}, base...), "error"),
	}, { // Test 1: A run that succeeded carries no reason, so the record is byte for byte what it was.
		Name: "succeeded", Status: run.StatusSucceeded, ExitCode: 0,
		WantError: "", WantKeys: base,
	}, { // Test 2: An honest failure the tool reported itself still records the detail it had.
		Name: "tool failed", Status: run.StatusFailed, ExitCode: 2, Error: "exit status 2",
		WantError: "exit status 2", WantKeys: append(append([]string{}, base...), "error"),
	}, { // Test 3: A failure with no text of its own, such as a cancel, adds no key either.
		Name: "no detail", Status: run.StatusCanceled, ExitCode: 0,
		WantError: "", WantKeys: base,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			exit := test.ExitCode
			r := &run.Run{
				ID: "run_reason", Playbook: "site.yml", Tool: run.ToolAnsible,
				Status: test.Status, ExitCode: &exit, Error: test.Error,
			}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			body, err := outcome.Body(ctx, store, r)
			if err != nil {
				t.Fatalf("outcome.Body() error = %v", err)
			}
			rec, err := outcome.Parse(body)
			if err != nil {
				t.Fatalf("outcome.Parse() error = %v", err)
			}
			if diff := cmp.Diff(test.WantError, rec.Error, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reason mismatch (-want +got):\n%s", diff)
			}
			want := append([]string{}, test.WantKeys...)
			sort.Strings(want)
			if diff := cmp.Diff(want, bodyKeys(t, body), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("outcome field set mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFailureReasonIsCoveredByTheCommitment proves the reason is committed rather than merely
// carried. A field a receipt shows but the chain does not fix is a field anyone holding the receipt
// can rewrite, so the reason would be worth no more than an assertion.
func TestFailureReasonIsCoveredByTheCommitment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	exit := 0
	r := &run.Run{
		ID: "run_committed", Playbook: "site.yml", Tool: run.ToolAnsible,
		Status: run.StatusFailed, ExitCode: &exit, Error: zeroHostReason,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	body, err := outcome.Body(ctx, store, r)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if !audit.VerifyContentDigest(digest, nonce, body) {
		t.Fatal("the outcome body does not verify against the digest taken over it")
	}

	// The same run, the same status, the same exit code, a softer reason. This is the rewrite the
	// commitment has to refuse.
	rewritten := rewriteReason(t, body, "the run completed with nothing to do")
	if audit.VerifyContentDigest(digest, nonce, rewritten) {
		t.Error("an outcome whose failure reason was rewritten verified against the committed digest")
	}
	// Dropping the reason must fail the same way, or a holder could publish the pair that says
	// nothing by deleting the field.
	dropped := rewriteReason(t, body, "")
	if audit.VerifyContentDigest(digest, nonce, dropped) {
		t.Error("an outcome whose failure reason was deleted verified against the committed digest")
	}
}

// TestFailureReasonIsCanonicalizedAndRedacted covers what the record admits when a failure detail
// is free text rather than a fixed sentence.
//
// Two things have to hold. The bytes must survive the LoomSeal JCS profile, since a reason the
// profile refuses would make every receipt for a failed run unsignable. And the reason must go
// through the same string-leaf redaction as every other field, so a runner error that quoted a
// command line with an inline credential cannot commit that credential into the chain: two failures
// differing only in the secret must reduce to the same committed bytes.
func TestFailureReasonIsCanonicalizedAndRedacted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Error string
		Twin  string
		Same  bool
	}{{ // Test 0: The zero-host sentence, punctuation and all.
		Name: "zero host sentence", Error: zeroHostReason,
	}, { // Test 1: A runner error carrying quotes, a newline, and a tab.
		Name: "quotes and control characters", Error: "exec \"ansible-playbook\":\n\tno such file",
	}, { // Test 2: Text outside ASCII.
		Name: "unicode",
		//nolint:misspell // French, which is the point: this case is non-ASCII input.
		Error: "connexion refusée: hôte inatteignable ✂",
	}, { // Test 3: An inline credential is redacted before it is digested, so the twin matches.
		Name: "inline credential", Error: "psql failed: password=hunter2",
		Twin: "psql failed: password=swordfish", Same: true,
	}, { // Test 4: The control. Two genuinely different reasons must not collide.
		Name: "different reasons", Error: "exit status 2", Twin: "exit status 3", Same: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			body := reasonBody(t, test.Error)
			if _, err := jcs.Canonicalize(body); err != nil {
				t.Fatalf("Canonicalize() refused an outcome carrying a failure reason: %v", err)
			}
			rec, err := outcome.Parse(body)
			if err != nil {
				t.Fatalf("outcome.Parse() error = %v", err)
			}
			if diff := cmp.Diff(test.Error, rec.Error, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reason mismatch (-want +got):\n%s", diff)
			}
			if test.Twin == "" {
				return
			}
			digest, nonce, err := audit.ContentDigestOf(body)
			if err != nil {
				t.Fatalf("ContentDigestOf() error = %v", err)
			}
			got := audit.VerifyContentDigest(digest, nonce, reasonBody(t, test.Twin))
			if got != test.Same {
				t.Errorf("twin reason verified = %v, want %v; the digest input is %q against %q",
					got, test.Same, test.Error, test.Twin)
			}
		})
	}
}

// reasonBody builds the outcome body of a failed run carrying reason, the shape every case here
// varies one field of.
func reasonBody(t *testing.T, reason string) []byte {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	exit := 0
	r := &run.Run{
		ID: "run_text", Playbook: "site.yml", Tool: run.ToolAnsible,
		Status: run.StatusFailed, ExitCode: &exit, Error: reason,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	body, err := outcome.Body(ctx, store, r)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	return body
}

// bodyKeys returns the sorted top-level field names an outcome body actually emits, which is what
// makes a claim about the record's field set checkable rather than read off the struct.
func bodyKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("the outcome body is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// rewriteReason returns body with its failure reason replaced, dropping the field when the
// replacement is empty. It is how a test forges the disclosure a receipt holder would forge.
func rewriteReason(t *testing.T, body []byte, reason string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("the outcome body is not a JSON object: %v", err)
	}
	if reason == "" {
		delete(fields, "error")
	} else {
		raw, err := json.Marshal(reason)
		if err != nil {
			t.Fatalf("Marshal(reason) error = %v", err)
		}
		fields["error"] = raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal(body) error = %v", err)
	}
	return out
}
