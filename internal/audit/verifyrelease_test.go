package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// releaseFixturesEnv regenerates the release fixtures under testdata when it is set to 1.
const releaseFixturesEnv = "SWITCHTENDER_REGENERATE_RELEASE_FIXTURES"

// Release fixtures, each a receipt a dispatcher over fresh stores produced for one workflow: build,
// an approval step, deploy behind its approval, and notify behind its denial, held for the
// workflow's own approval first.
const (
	// releaseApproved is the receipt of a keyed workflow whose approval step was approved.
	releaseApproved = "release-approved.loomseal.json"
	// releaseDenied is the receipt of a keyed workflow whose approval step was rejected.
	releaseDenied = "release-denied.loomseal.json"
	// releaseTimedOut is the receipt of a keyed workflow whose approval step timed out.
	releaseTimedOut = "release-timed-out.loomseal.json"
	// releaseWithheld is the approved receipt built after retention purged the workflow's logs, so
	// it withholds the outcome the chain committed and still discloses its releases.
	releaseWithheld = "release-withheld.loomseal.json"
	// releaseUnkeyed is the approved workflow's receipt from a dispatcher with no binding key, which
	// records no releases.
	releaseUnkeyed = "release-unkeyed.loomseal.json"
)

// releaseSeed is the seed of the producer key the release fixtures are signed with. It is derived
// rather than written down, so a row can sign an edited fixture again as its producer would.
func releaseSeed() []byte {
	sum := sha256.Sum256([]byte("switchtender release fixtures producer"))
	return sum[:]
}

// releaseKey returns the producer key the release fixtures are signed with.
func releaseKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(releaseSeed())
}

// releaseScenario is one workflow a release fixture is generated from.
type releaseScenario struct {
	// File is the fixture written under testdata.
	File string
	// Keyed gives the dispatcher a binding key, so it records a release for each step it launches.
	Keyed bool
	// Verdict settles the approval step: approve, deny, or timeout, which leaves it to the clock.
	Verdict string
	// Prune purges the workflow's logs once the outcome is committed, the way retention changes
	// what an outcome rebuilds to, so the receipt withholds the outcome.
	Prune bool
}

// TestReleaseFixturesRegenerate writes the receipts the release rows read when releaseFixturesEnv
// is 1, and is skipped otherwise. Each is the receipt a real dispatcher and a real chain produce
// for a workflow run end to end, so the rows hold the verifier to what this product emits rather
// than to a document written by hand.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestReleaseFixturesRegenerate(t *testing.T) {
	if os.Getenv(releaseFixturesEnv) != "1" {
		t.Skip("set " + releaseFixturesEnv + "=1 to regenerate the release fixtures")
	}
	t.Setenv("SWITCHTENDER_AUDIT_KEY", hex.EncodeToString(releaseSeed()))
	scenarios := []releaseScenario{
		{File: releaseApproved, Keyed: true, Verdict: "approve"},
		{File: releaseDenied, Keyed: true, Verdict: "deny"},
		{File: releaseTimedOut, Keyed: true, Verdict: "timeout"},
		{File: releaseWithheld, Keyed: true, Verdict: "approve", Prune: true},
		{File: releaseUnkeyed, Verdict: "approve"},
	}
	for _, s := range scenarios {
		signed := generateReleaseReceipt(t, s)
		if err := os.WriteFile(filepath.Join("testdata", s.File), signed, 0o644); err != nil {
			t.Fatalf("write %s: %v", s.File, err)
		}
	}
}

// releaseSteps is the workflow every release fixture runs. A positive timeout makes the approval
// step time out after that many seconds.
func releaseSteps(timeout int) []run.PipelineStep {
	return []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "make"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", ApprovalTimeout: timeout,
			DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "ship", DependsOn: []string{"gate"}},
		{Name: "notify", Tool: run.ToolBash, Command: "page", IfDenied: []string{"gate"}},
	}
}

// generateReleaseReceipt runs one scenario's workflow through a dispatcher over fresh stores and
// returns the receipt for it, signed by the identity the audit key environment names.
//
//nolint:funlen // Test helper.
func generateReleaseReceipt(t *testing.T, s releaseScenario) []byte {
	t.Helper()
	ctx := context.Background()
	runs, audits := run.NewMemStore(), audit.NewMemStore()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "requester",
		ActorType: "session", Method: "POST", Path: "/v1/pipelines"}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, err := io.WriteString(out, "done\n")
			return roundhouse.Result{ExitCode: 0}, err
		},
	)
	opts := []dispatch.Option{dispatch.WithAudits(audits)}
	if s.Keyed {
		bindingSeed := sha256.Sum256([]byte("switchtender release fixtures binding"))
		key, err := binding.DeriveKey(bindingSeed[:])
		if err != nil {
			t.Fatalf("DeriveKey() error = %v", err)
		}
		opts = append(opts, dispatch.WithBindingKey(key))
	}
	d := dispatch.New(runs, runner, nil, append(slices.Clone(opts), dispatch.WithNoJanitor())...)
	timeout := 0
	if s.Verdict == "timeout" {
		timeout = 1
	}
	parent, err := d.SubmitPipeline(ctx, "release", "", releaseSteps(timeout),
		run.WithActor("requester"), run.WithRequireApproval(true),
		run.WithAuditReceiptOf(fmt.Sprintf("%d:%s", creation.Seq, creation.Hash)))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	approver := outcome.Decider{Name: "approver", Type: "session"}
	if _, err := d.Approve(ctx, parent.ID, approver); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	node := waitForGate(t, runs, parent.ID)
	if s.Verdict == "timeout" {
		// The sweep a dispatcher makes as it starts times out a step whose wait has passed.
		time.Sleep(time.Until(node.CreatedAt.Add(1500 * time.Millisecond)))
		d.Close()
		d = dispatch.New(runs, runner, nil, opts...)
	} else if _, err := d.DecideStep(ctx, node.ID, dispatch.StepDecision{
		Approve: s.Verdict == "approve", By: approver}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	defer d.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := runs.Get(ctx, parent.ID)
		if err == nil && got.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow %s never ended", parent.ID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The outcome entry is appended as the workflow ends, so the receipt is built once it is there.
	var res *receipt.Result
	for res == nil {
		if res, err = receipt.Build(ctx, runs, audits, id, "v-test", parent.ID,
			receipt.Options{}); err != nil && time.Now().After(deadline) {
			t.Fatalf("receipt.Build() error = %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !s.Prune {
		return res.Signed
	}
	if _, err := runs.PurgeEventsBefore(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	res, err = receipt.Build(ctx, runs, audits, id, "v-test", parent.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("the receipt still discloses the outcome after its logs were purged")
	}
	return res.Signed
}

// waitForGate waits until the workflow is parked at its approval step and returns the step.
func waitForGate(t *testing.T, runs run.Store, parentID string) *run.Run {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		parent, err := runs.Get(ctx, parentID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		pending, err := run.PendingApprovalSteps(ctx, runs, parentID)
		if err != nil {
			t.Fatalf("PendingApprovalSteps() error = %v", err)
		}
		if parent.Status == run.StatusPendingApproval && parent.ClaimedBy == "" && len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the workflow never parked at its approval step")
	return nil
}

// releaseResult is what the verifier reported about a receipt's releases.
type releaseResult struct {
	// OK is the whole verdict.
	OK bool
	// ReleasesOK reports every disclosed release matched its entry and disclosed no reason.
	ReleasesOK bool
	// Consistent reports every verified release held to the outcome and to its decisions.
	Consistent bool
	// Compared reports the releases were held to a verified outcome's children.
	Compared bool
	// Entries counts the release entries the receipt carries.
	Entries int
	// Present counts the releases it disclosed.
	Present int
	// Checked counts the release bodies reported checked.
	Checked int
	// Released names each verified release by its step and the verdicts of the decisions it names.
	Released []string
}

// TestReleaseClaimsAreHeldToTheOutcome pins how a receipt's step releases are judged. Each release
// body is held to the content digest its entry committed. Its spec digest is held to the one the
// verified outcome names for the step record it released, at the same index and attempt, and each
// decision it names must be found by content digest on exactly one claim that is a verified
// decision of the same run that approved, rejected, or timed out. A receipt that withholds its
// outcome holds its releases to none and still verifies, and so does one that discloses a release
// entry without its body. A release commits no reason, so a reason disclosed beside one fails. A
// receipt with no releases is judged exactly as before releases existed.
//
// Every row starts from a receipt a dispatcher produced. A row that edits a record commits the
// edited body under a fresh digest and relinks the chain the way its producer would, and signs the
// receipt again with the producer's key, so the only thing wrong with it is what the row changed.
//
//nolint:funlen // Test function.
func TestReleaseClaimsAreHeldToTheOutcome(t *testing.T) {
	t.Parallel()
	approvedReleases := []string{"build approved", "deploy approved approved"}
	tests := []struct {
		// Fixture is the receipt the row starts from.
		Fixture string
		// Edit changes the receipt before it is signed again, nil to verify it as produced.
		Edit func(t *testing.T, doc *audit.Bundle)
		// WantResult is what the verifier reports about the releases.
		WantResult releaseResult
		// WantProblem is a phrase the release problems carry, empty for none.
		WantProblem string
	}{{ // Test 0: An approved workflow's receipt verifies with each release held to the outcome.
		Fixture: releaseApproved,
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true, Compared: true,
			Entries: 2, Present: 2, Checked: 2, Released: approvedReleases},
	}, { // Test 1: A denied workflow's deny path verifies, released by the rejection.
		Fixture: releaseDenied,
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true, Compared: true,
			Entries: 2, Present: 2, Checked: 2,
			Released: []string{"build approved", "notify approved rejected"}},
	}, { // Test 2: A deny path the clock took verifies, released by the timeout.
		Fixture: releaseTimedOut,
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true, Compared: true,
			Entries: 2, Present: 2, Checked: 2,
			Released: []string{"build approved", "notify approved timed_out"}},
	}, { // Test 3: A receipt that withholds its outcome verifies its releases and compares none.
		Fixture: releaseWithheld,
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true,
			Entries: 2, Present: 2, Checked: 2, Released: approvedReleases},
	}, { // Test 4: A release whose spec differs from the one the outcome names for its step fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["spec_digest"] = "sha256:" + strings.Repeat("ab", 32)
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "and the outcome names spec",
	}, { // Test 5: A release naming a decision by a digest no claim commits fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			rootAt(body, 0)["content_digest"] = "sha256s:" + strings.Repeat("0", 64) + ":" +
				strings.Repeat("1", 64)
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: []string{"build approved", "deploy  approved"}},
		WantProblem: "by a digest no claim here commits",
	}, { // Test 6: A release naming the approval step's request, which decides nothing, fails.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			request := claimDigest(t, doc, decisionClaim(t, doc, "/decision/requested"))
			editRelease("deploy", func(body map[string]any) {
				rootAt(body, 0)["content_digest"] = request
			})(t, doc)
		},
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: []string{"build approved", "deploy approved requested"}},
		WantProblem: `whose verdict "requested" releases no step`,
	}, { // Test 7: A release whose decisions decided another run than the one it names fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["run_id"] = "run_elsewhere"
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "not run run_elsewhere",
	}, { // Test 8: A release of a step record the outcome does not name fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["step_run_id"] = "run_missing"
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "releases step record run_missing, which the outcome does not name",
	}, { // Test 9: A reason disclosed beside a release fails, since a release commits none.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			doc.Claims[releaseClaim(t, doc, "deploy")].Payload["reason_text"] = "it looked fine"
		},
		WantResult: releaseResult{Consistent: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "discloses reason_text beside its release_body",
	}, { // Test 10: A release body carrying a reason commitment fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["reason_commitment"] = "sha256:" + strings.Repeat("cd", 32)
		}),
		WantResult: releaseResult{Consistent: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "release_body carries a reason_commitment",
	}, { // Test 11: A release body changed without its digest fails against the entry.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			body := doc.Claims[releaseClaim(t, doc, "deploy")].Payload["release_body"]
			body.(map[string]any)["attempt"] = float64(7)
		},
		WantResult: releaseResult{Consistent: true, Compared: true, Entries: 2, Present: 2,
			Checked: 1, Released: []string{"build approved"}},
		WantProblem: "release_body does not match the content digest its entry committed",
	}, { // Test 12: Release entries carried without their bodies are counted and fail nothing.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			for _, step := range []string{"build", "deploy"} {
				payload := doc.Claims[releaseClaim(t, doc, step)].Payload
				delete(payload, "release_body")
				delete(payload, "release_nonce")
			}
		},
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true, Entries: 2},
	}, { // Test 13: A receipt with no releases is judged as before releases existed.
		Fixture:    releaseUnkeyed,
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true},
	}, { // Test 14: A release body that names its decisions in no form a release carries fails.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["roots"] = "approved"
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: []string{"build approved", "deploy"}},
		WantProblem: "does not name its decisions as a release record does",
	}, { // Test 15: A release naming the outcome, which is no decision, fails.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			ran := claimDigest(t, doc, outcomeClaim(t, doc))
			editRelease("deploy", func(body map[string]any) {
				rootAt(body, 0)["content_digest"] = ran
			})(t, doc)
		},
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: []string{"build approved", "deploy  approved"}},
		WantProblem: "is not a verified decision",
	}, { // Test 16: An outcome committed as its summary names no steps, so none is compared.
		Fixture: releaseApproved,
		Edit: func(t *testing.T, doc *audit.Bundle) {
			at := outcomeClaim(t, doc)
			var full map[string]any
			text, _ := doc.Claims[at].Payload["outcome_body"].(string)
			if err := json.Unmarshal([]byte(text), &full); err != nil {
				t.Fatalf("decode the outcome: %v", err)
			}
			summary := map[string]any{"oversize": map[string]any{"limit": 1 << 20,
				"sha256": strings.Repeat("e", 64), "size": 2 << 20}}
			for _, k := range []string{"run_id", "status", "exit_code", "spec_digest"} {
				summary[k] = full[k]
			}
			raw, err := json.Marshal(summary)
			if err != nil {
				t.Fatalf("encode the summary: %v", err)
			}
			digest, nonce, err := audit.ExactDigestOf(raw)
			if err != nil {
				t.Fatalf("ExactDigestOf() error = %v", err)
			}
			payload := doc.Claims[at].Payload
			payload["outcome_body"], payload["outcome_nonce"] = string(raw), nonce
			payload["content_digest"] = digest
			relink(t, doc, at)
		},
		WantResult: releaseResult{OK: true, ReleasesOK: true, Consistent: true, Entries: 2,
			Present: 2, Checked: 2, Released: approvedReleases},
	}, { // Test 17: A release whose entry's path names another step record than its body fails.
		Fixture: releaseApproved,
		Edit: editReleasePath("deploy", func(runID, _ string) string {
			return audit.ReleasePath(runID, "run_other")
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "and its entry's path names run_other",
	}, { // Test 18: A release whose entry's path names another workflow than its body fails.
		Fixture: releaseApproved,
		Edit: editReleasePath("deploy", func(_, stepRunID string) string {
			return audit.ReleasePath("run_elsewhere", stepRunID)
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: approvedReleases},
		WantProblem: "and its entry's path names run run_elsewhere",
	}, { // Test 19: A release naming no decision at all fails, since no decision made it.
		Fixture: releaseApproved,
		Edit: editRelease("deploy", func(body map[string]any) {
			body["roots"] = []any{}
		}),
		WantResult: releaseResult{ReleasesOK: true, Compared: true, Entries: 2, Present: 2,
			Checked: 2, Released: []string{"build approved", "deploy"}},
		WantProblem: "release_body names no decision",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			signed, err := os.ReadFile(filepath.Join("testdata", test.Fixture))
			if err != nil {
				t.Fatalf("read the fixture: %v", err)
			}
			if test.Edit != nil {
				var doc audit.Bundle
				if err := json.Unmarshal(signed, &doc); err != nil {
					t.Fatalf("decode the fixture: %v", err)
				}
				test.Edit(t, &doc)
				if signed, err = audit.SignBundleDoc(&doc, releaseKey()); err != nil {
					t.Fatalf("SignBundleDoc() error = %v", err)
				}
			}
			pub, _ := releaseKey().Public().(ed25519.PublicKey)
			rep, err := audit.VerifyBundle(signed, seal.KeyID(pub))
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if !rep.SignatureOK || !rep.ChainOK || !rep.DecisionsOK || !rep.SpecConsistent {
				t.Fatalf("the row broke more than the releases: signature %v, chain %v (%s), "+
					"decisions %v, spec %v", rep.SignatureOK, rep.ChainOK, rep.ChainProblem,
					rep.DecisionsOK, rep.SpecConsistent)
			}
			got := releaseResult{OK: rep.OK(), ReleasesOK: rep.ReleasesOK,
				Consistent: rep.ReleasesConsistent, Compared: rep.ReleasesCompared,
				Entries: rep.ReleaseEntries, Present: rep.ReleasesPresent}
			for _, m := range rep.Disclosed {
				if m.Member == "release_body" && m.State == audit.MemberChecked {
					got.Checked++
				}
			}
			for _, r := range rep.Releases {
				verdicts := make([]string, 0, len(r.Roots))
				for _, root := range r.Roots {
					verdicts = append(verdicts, root.Verdict)
				}
				sort.Strings(verdicts)
				got.Released = append(got.Released, strings.Join(append([]string{r.Step},
					verdicts...), " "))
			}
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("release verdict mismatch (-want +got):\n%s", diff)
			}
			problems := strings.Join(rep.ReleaseProblems, "; ")
			switch {
			case test.WantProblem == "" && problems != "":
				t.Errorf("release problems = %q, want none", problems)
			case !strings.Contains(problems, test.WantProblem):
				t.Errorf("release problems = %q, want one saying %q", problems, test.WantProblem)
			}
		})
	}
}

// editRelease returns an edit that changes the body of the named step's release and commits the
// changed body as the producer would.
func editRelease(step string, change func(body map[string]any)) func(*testing.T, *audit.Bundle) {
	return func(t *testing.T, doc *audit.Bundle) {
		t.Helper()
		at := releaseClaim(t, doc, step)
		body, ok := doc.Claims[at].Payload["release_body"].(map[string]any)
		if !ok {
			t.Fatalf("the %s release discloses no body", step)
		}
		change(body)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the release: %v", err)
		}
		digest, nonce, err := audit.ContentDigestOf(raw)
		if err != nil {
			t.Fatalf("ContentDigestOf() error = %v", err)
		}
		payload := doc.Claims[at].Payload
		payload["content_digest"], payload["release_nonce"] = digest, nonce
		relink(t, doc, at)
	}
}

// editReleasePath returns an edit that moves the named step's release entry to the path pathOf
// builds from the workflow and step record its body names, and relinks the chain over it the way
// the producer would, leaving the body and its digest as committed.
func editReleasePath(step string, pathOf func(runID, stepRunID string) string) func(*testing.T,
	*audit.Bundle) {
	return func(t *testing.T, doc *audit.Bundle) {
		t.Helper()
		at := releaseClaim(t, doc, step)
		payload := doc.Claims[at].Payload
		body, _ := payload["release_body"].(map[string]any)
		runID, _ := body["run_id"].(string)
		stepRunID, _ := body["step_run_id"].(string)
		payload["path"] = pathOf(runID, stepRunID)
		relink(t, doc, at)
	}
}

// rootAt returns the root at index i of a release body.
func rootAt(body map[string]any, i int) map[string]any {
	roots, _ := body["roots"].([]any)
	root, _ := roots[i].(map[string]any)
	return root
}

// releaseClaim returns the index of the claim releasing the named step.
func releaseClaim(t *testing.T, doc *audit.Bundle, step string) int {
	t.Helper()
	for i, c := range doc.Claims {
		body, _ := c.Payload["release_body"].(map[string]any)
		if c.Payload["method"] == audit.MethodRelease && body["step"] == step {
			return i
		}
	}
	t.Fatalf("the receipt discloses no release of %s", step)
	return -1
}

// decisionClaim returns the index of the decision claim whose path ends in suffix.
func decisionClaim(t *testing.T, doc *audit.Bundle, suffix string) int {
	t.Helper()
	for i, c := range doc.Claims {
		path, _ := c.Payload["path"].(string)
		if c.Payload["method"] == audit.MethodDecision && strings.HasSuffix(path, suffix) {
			return i
		}
	}
	t.Fatalf("the receipt has no decision ending in %s", suffix)
	return -1
}

// outcomeClaim returns the index of the claim disclosing the workflow's outcome.
func outcomeClaim(t *testing.T, doc *audit.Bundle) int {
	t.Helper()
	for i, c := range doc.Claims {
		if _, ok := c.Payload["outcome_body"]; ok {
			return i
		}
	}
	t.Fatal("the receipt discloses no outcome")
	return -1
}

// claimDigest returns the content digest claim i commits.
func claimDigest(t *testing.T, doc *audit.Bundle, i int) string {
	t.Helper()
	digest, _ := doc.Claims[i].Payload["content_digest"].(string)
	if digest == "" {
		t.Fatalf("claim %d commits no content digest", i)
	}
	return digest
}

// relink recomputes the chain link of every claim from claim from on, each over the one before it,
// and the head the last one stands at, the way the producer computed them. It first recomputes the
// claim's link as it stood, so a relink that would not reproduce the producer's own links fails the
// row instead of passing it on links nobody wrote.
func relink(t *testing.T, doc *audit.Bundle, from int) {
	t.Helper()
	link := func(c *audit.BundleClaim) string {
		at, err := time.Parse(time.RFC3339Nano, c.At)
		if err != nil {
			t.Fatalf("claim at %q: %v", c.At, err)
		}
		str := func(k string) string { s, _ := c.Payload[k].(string); return s }
		return audit.EntryHash(&audit.Entry{Seq: c.Chain.Seq, At: at, Actor: str("actor"),
			ActorType: str("actor_type"), OnBehalfOf: str("on_behalf_of"), Method: str("method"),
			Path: str("path"), ContentDigest: str("content_digest"), InstallID: str("install_id"),
			PrevHash: c.Chain.Prev})
	}
	if from > 0 {
		if prev := &doc.Claims[from-1]; link(prev) != prev.Chain.Link {
			t.Fatal("the relink does not reproduce the producer's links")
		}
	}
	for i := from; i < len(doc.Claims); i++ {
		c := &doc.Claims[i]
		if i > 0 {
			c.Chain.Prev = doc.Claims[i-1].Chain.Link
		}
		c.Chain.Link = link(c)
	}
	last := doc.Claims[len(doc.Claims)-1].Chain
	if doc.Chain != nil && doc.Chain.Head.Seq == last.Seq {
		doc.Chain.Head.Link = last.Link
	}
}
