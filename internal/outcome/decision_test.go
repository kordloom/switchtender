package outcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// decisionRun returns the run an approver is deciding on, with every spec-bearing field filled so a
// test that changes one is changing something the digest already covered.
func decisionRun() *run.Run {
	shards := 4
	return &run.Run{
		ID: "run_decide", Tool: "ansible", Playbook: "site.yml", Command: "",
		Inventory: "prod.ini", InventoryID: "inv_1", ProjectID: "proj_1",
		Limit: "web*", Tags: []string{"deploy"}, SkipTags: []string{"slow"},
		ExtraVars:     map[string]any{"release": "2026.09"},
		CredentialIDs: []string{"cred_1"}, PullCredentialID: "cred_pull",
		DryRun: false, Verbosity: 2, Forks: 10, DiffMode: true, Timeout: 900,
		ShardCount: &shards, Status: run.StatusPendingApproval, Actor: "operator-jane",
	}
}

// TestDecisionBodyCommitsTheRunTheVerdictAndTheSpec pins the exact bytes an approval commits. The
// decision is what releases a change onto production, so the body has to name the run, the verdict,
// and the digest of the spec decided on; without the digest an approval names a run id whose
// meaning the evidence cannot pin down, and a spec swapped after the decision would still look
// approved.
func TestDecisionBodyCommitsTheRunTheVerdictAndTheSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Verdict is what the approver decided.
		Verdict string
	}{
		{Name: "approved", Verdict: "approved"}, // Test 0: The verdict that releases the run.
		{Name: "rejected", Verdict: "rejected"}, // Test 1: The verdict that stops it.
		{Name: "empty", Verdict: ""},            // Test 2: A caller that named no verdict at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := decisionRun()
			body, specDigest, err := DecisionBody(r, test.Verdict)
			if err != nil {
				t.Fatalf("DecisionBody() error = %v", err)
			}

			wantDigest, err := SpecDigest(r)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			if specDigest != wantDigest {
				t.Errorf("returned spec digest = %q, want the run's own %q", specDigest, wantDigest)
			}
			if !strings.HasPrefix(specDigest, "sha256:") {
				t.Errorf("spec digest = %q, want the unkeyed form a receipt holder recomputes",
					specDigest)
			}

			var got DecisionRecord
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("the decision body is not the record it claims to be: %v", err)
			}
			want := DecisionRecord{RunID: r.ID, Verdict: test.Verdict, SpecDigest: wantDigest}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("decision record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDecisionBodyIsReproducible proves two calls over the same run produce the same bytes. A
// receipt rebuilds this body to check it against the digest the chain committed, so a body that
// differed run to run would make every approval report as unverifiable.
func TestDecisionBodyIsReproducible(t *testing.T) {
	t.Parallel()
	first, firstDigest, err := DecisionBody(decisionRun(), "approved")
	if err != nil {
		t.Fatalf("DecisionBody() error = %v", err)
	}
	second, secondDigest, err := DecisionBody(decisionRun(), "approved")
	if err != nil {
		t.Fatalf("DecisionBody() error = %v", err)
	}
	if diff := cmp.Diff(string(first), string(second)); diff != "" {
		t.Errorf("the same run produced two decision bodies (-first +second):\n%s", diff)
	}
	if firstDigest != secondDigest {
		t.Errorf("spec digests differ: %q then %q", firstDigest, secondDigest)
	}
}

// TestDecisionBodyBindsToEveryFieldThatChangesWhatRuns is the substitution check the whole approval
// mechanism rests on. The executor recomputes the spec digest and refuses a mismatch, so a field
// that changes what the run does to a system and is not covered by the digest is a field an
// approval can be lifted from one change and dropped onto another.
//
// The excluded fields are excluded on purpose and are pinned here for the same reason: the image is
// resolved onto the run after approval by a project or server default, so including it would make
// every such run fail its own check, and the resolved image is committed by the outcome record
// instead. Scheduling and provenance say where and why a run was asked for rather than what it
// does.
func TestDecisionBodyBindsToEveryFieldThatChangesWhatRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the field.
		Name string
		// Mutate changes the run the way a substitution would.
		Mutate func(r *run.Run)
		// WantBound is whether the digest must change, refusing the substitution.
		WantBound bool
	}{{ // Test 0: The engine the run executes with.
		Name: "tool", Mutate: func(r *run.Run) { r.Tool = "bash" }, WantBound: true,
	}, { // Test 1: The playbook, which is the change itself.
		Name: "playbook", Mutate: func(r *run.Run) { r.Playbook = "destroy.yml" }, WantBound: true,
	}, { // Test 2: The command a non-Ansible tool runs, which is also the change itself.
		Name: "command", Mutate: func(r *run.Run) { r.Command = "rm -rf /" }, WantBound: true,
	}, { // Test 3: The inventory, which decides which machines are touched.
		Name: "inventory", Mutate: func(r *run.Run) { r.Inventory = "everything.ini" },
		WantBound: true,
	}, { // Test 4: The stored inventory, the same decision by reference.
		Name: "inventory id", Mutate: func(r *run.Run) { r.InventoryID = "inv_prod_all" },
		WantBound: true,
	}, { // Test 5: The project, which is where the content comes from.
		Name: "project id", Mutate: func(r *run.Run) { r.ProjectID = "proj_other" }, WantBound: true,
	}, { // Test 6: The limit, which narrows or widens the blast radius.
		Name: "limit", Mutate: func(r *run.Run) { r.Limit = "*" }, WantBound: true,
	}, { // Test 7: Tags select which plays run.
		Name: "tags", Mutate: func(r *run.Run) { r.Tags = []string{"deploy", "migrate"} },
		WantBound: true,
	}, { // Test 8: Skip tags deselect them, which is the same power.
		Name: "skip tags", Mutate: func(r *run.Run) { r.SkipTags = nil }, WantBound: true,
	}, { // Test 9: Extra vars are injected into the run and change what it does.
		Name: "extra vars", Mutate: func(r *run.Run) { r.ExtraVars["release"] = "2019.01" },
		WantBound: true,
	}, { // Test 10: An extra var added after the decision is a change to the run.
		Name:   "extra var added",
		Mutate: func(r *run.Run) { r.ExtraVars["force"] = true }, WantBound: true,
	}, { // Test 11: The credentials the run executes with decide what it may reach.
		Name: "credential ids", Mutate: func(r *run.Run) { r.CredentialIDs = []string{"cred_root"} },
		WantBound: true,
	}, { // Test 12: The registry credential for a private image.
		Name: "pull credential id", Mutate: func(r *run.Run) { r.PullCredentialID = "cred_other" },
		WantBound: true,
	}, { // Test 13: Turning a preview into a real change is the substitution this exists to stop.
		Name: "dry run", Mutate: func(r *run.Run) { r.DryRun = true }, WantBound: true,
	}, { // Test 14: Verbosity changes what a run prints, which is what a log then holds.
		Name: "verbosity", Mutate: func(r *run.Run) { r.Verbosity = 4 }, WantBound: true,
	}, { // Test 15: Forks change how many hosts are addressed at once.
		Name: "forks", Mutate: func(r *run.Run) { r.Forks = 200 }, WantBound: true,
	}, { // Test 16: Diff mode prints the before and after of every file change.
		Name: "diff mode", Mutate: func(r *run.Run) { r.DiffMode = false }, WantBound: true,
	}, { // Test 17: The timeout bounds how long the change may run for.
		Name: "timeout", Mutate: func(r *run.Run) { r.Timeout = 1 }, WantBound: true,
	}, { // Test 18: The shard count is how wide a split fans out.
		Name: "shard count", Mutate: func(r *run.Run) { n := 64; r.ShardCount = &n },
		WantBound: true,
	}, { // Test 19: Clearing the shard count entirely.
		Name: "shard count cleared", Mutate: func(r *run.Run) { r.ShardCount = nil },
		WantBound: true,
	}, { // Test 20: The pipeline graph is the whole of what a pipeline does.
		Name: "steps",
		Mutate: func(r *run.Run) {
			r.Steps = []run.PipelineStep{{Name: "s", Playbook: "p.yml"}}
		},
		WantBound: true,
	}, { // Test 21: The image is resolved after approval by a project or server default, so binding
		// it would fail every run that used one. The outcome record commits what actually ran.
		Name: "image", Mutate: func(r *run.Run) { r.Image = "evil:latest" }, WantBound: false,
	}, { // Test 22: The run id names the decision, not the spec, and it is committed separately.
		Name: "run id", Mutate: func(r *run.Run) { r.ID = "run_other" }, WantBound: false,
	}, { // Test 23: Who asked is provenance, not content.
		Name: "actor", Mutate: func(r *run.Run) { r.Actor = "someone-else" }, WantBound: false,
	}, { // Test 24: Which worker pool serves the run is scheduling, not content.
		Name: "queue", Mutate: func(r *run.Run) { r.Queue = "isolated" }, WantBound: false,
	}, { // Test 25: Labels are for slicing runs in a list.
		Name: "labels", Mutate: func(r *run.Run) { r.Labels = map[string]string{"env": "prod"} },
		WantBound: false,
	}, { // Test 26: The commit is stamped by the project sync after the decision.
		Name: "commit sha", Mutate: func(r *run.Run) { r.CommitSHA = "deadbeef" }, WantBound: false,
	}}
	base, err := SpecDigest(decisionRun())
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := decisionRun()
			test.Mutate(r)
			_, got, err := DecisionBody(r, "approved")
			if err != nil {
				t.Fatalf("DecisionBody() error = %v", err)
			}
			if changed := got != base; changed != test.WantBound {
				t.Errorf("changing %s moved the spec digest = %v, want %v; a field the digest does "+
					"not cover can be changed after approval and still pass the executor's check",
					test.Name, changed, test.WantBound)
			}
		})
	}
}

// TestSpecDigestIgnoresEmptyAndAbsentAlike pins that a field left unset and a field set to its zero
// value produce the same digest. They mean the same thing to the executor, so a decision taken on
// one must release the other; treating them as different would refuse a run nobody changed.
func TestSpecDigestIgnoresEmptyAndAbsentAlike(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the pair.
		Name string
		// Left and Right are two spellings of the same request.
		Left, Right func(r *run.Run)
	}{{ // Test 0: An empty tag slice is no tags.
		Name: "empty tags",
		Left: func(r *run.Run) { r.Tags = nil }, Right: func(r *run.Run) { r.Tags = []string{} },
	}, { // Test 1: An empty extra vars map is no extra vars.
		Name:  "empty extra vars",
		Left:  func(r *run.Run) { r.ExtraVars = nil },
		Right: func(r *run.Run) { r.ExtraVars = map[string]any{} },
	}, { // Test 2: A nil shard count and a zero one both mean no split.
		Name:  "zero shard count",
		Left:  func(r *run.Run) { r.ShardCount = nil },
		Right: func(r *run.Run) { n := 0; r.ShardCount = &n },
	}, { // Test 3: An empty credential list is no credentials.
		Name:  "empty credentials",
		Left:  func(r *run.Run) { r.CredentialIDs = nil },
		Right: func(r *run.Run) { r.CredentialIDs = []string{} },
	}, { // Test 4: An empty step list is not a pipeline.
		Name:  "empty steps",
		Left:  func(r *run.Run) { r.Steps = nil },
		Right: func(r *run.Run) { r.Steps = []run.PipelineStep{} },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			left, right := decisionRun(), decisionRun()
			test.Left(left)
			test.Right(right)
			leftDigest, err := SpecDigest(left)
			if err != nil {
				t.Fatalf("SpecDigest(left) error = %v", err)
			}
			rightDigest, err := SpecDigest(right)
			if err != nil {
				t.Fatalf("SpecDigest(right) error = %v", err)
			}
			if leftDigest != rightDigest {
				t.Errorf("%s digests differ: %q and %q, so an approval taken on one spelling "+
					"refuses the other", test.Name, leftDigest, rightDigest)
			}
		})
	}
}

// TestSpecDigestIsIndependentOfMapOrder proves the digest does not move with the order Go happens
// to walk a map. Extra vars are a map, and a digest that changed between two builds of the same run
// would make an approved run fail the executor's check at random.
func TestSpecDigestIsIndependentOfMapOrder(t *testing.T) {
	t.Parallel()
	vars := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	first := decisionRun()
	first.ExtraVars = vars
	want, err := SpecDigest(first)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	for i := 0; i < 50; i++ {
		other := decisionRun()
		other.ExtraVars = map[string]any{}
		for k, v := range vars {
			other.ExtraVars[k] = v
		}
		got, err := SpecDigest(other)
		if err != nil {
			t.Fatalf("SpecDigest() error = %v", err)
		}
		if got != want {
			t.Fatalf("iteration %d produced digest %q, want the stable %q", i, got, want)
		}
	}
}

// TestSpecRedactsSecretsBeforeAnyCallerHoldsTheBytes proves redaction happens inside Spec rather
// than at the point of disclosure. The bytes are shown beside their digest in a receipt, so a
// caller that could obtain unredacted bytes would be handing an outside auditor a password under a
// run's own variables, and the digest would commit it too.
func TestSpecRedactsSecretsBeforeAnyCallerHoldsTheBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels where the secret sits.
		Name string
		// Vars are the run's injected variables.
		Vars map[string]any
		// Secret is the value that must not survive.
		Secret string
	}{{ // Test 0: A secret under a name the classifier knows.
		Name: "named secret", Vars: map[string]any{"ansible_password": "hunter2"}, Secret: "hunter2",
	}, { // Test 1: A secret joined onto an ordinary variable's value, the shape a command line takes.
		Name: "inside a command",
		Vars: map[string]any{"deploy_cmd": "psql --password=hunter2 -h db"}, Secret: "hunter2",
	}, { // Test 2: A credential embedded in a URL, which no name=value pattern names.
		Name:   "url credential",
		Vars:   map[string]any{"dump_cmd": "pg_dump postgres://backup:hunter2@db/prod"},
		Secret: "hunter2",
	}, { // Test 3: A secret nested inside a structure, since extra vars are arbitrary JSON.
		Name: "nested",
		Vars: map[string]any{"outer": map[string]any{"api_key": "hunter2"}}, Secret: "hunter2",
	}, { // Test 4: A secret inside a list.
		Name: "in a list",
		Vars: map[string]any{"args": []any{"--token=hunter2"}}, Secret: "hunter2",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := decisionRun()
			r.ExtraVars = test.Vars
			raw, err := Spec(r)
			if err != nil {
				t.Fatalf("Spec() error = %v", err)
			}
			if strings.Contains(string(raw), test.Secret) {
				t.Errorf("Spec() = %s, which is disclosed beside its digest in a receipt",
					raw)
			}
			body, _, err := DecisionBody(r, "approved")
			if err != nil {
				t.Fatalf("DecisionBody() error = %v", err)
			}
			if strings.Contains(string(body), test.Secret) {
				t.Errorf("DecisionBody() = %s, which the chain commits", body)
			}
		})
	}
}

// TestDecisionBodyFailsClosedOnAnUnencodableSpec proves a run whose spec cannot be reduced to bytes
// produces no decision at all. Extra vars carry arbitrary values, and a value JSON cannot represent
// has to stop the approval rather than yield a body missing the part that would not encode: an
// approval that committed a partial spec would bind to something other than what runs.
func TestDecisionBodyFailsClosedOnAnUnencodableSpec(t *testing.T) {
	t.Parallel()
	r := decisionRun()
	r.ExtraVars = map[string]any{"ratio": math.NaN()}

	body, specDigest, err := DecisionBody(r, "approved")
	if err == nil {
		t.Fatalf("DecisionBody() = %s, nil error, want the unencodable spec refused", body)
	}
	if body != nil || specDigest != "" {
		t.Errorf("DecisionBody() returned body %q and digest %q beside its error, want neither",
			body, specDigest)
	}
}

// failingAudits is an audit store that cannot append, the shape a store takes when its database is
// unreachable or its chain is locked.
type failingAudits struct {
	audit.Store
	// appends counts the entries it was asked to write.
	appends int
}

// errAppend stands for a chain that cannot be written to.
var errAppend = errors.New("the audit chain is unwritable")

// Append refuses, counting the attempt.
func (f *failingAudits) Append(context.Context, *audit.Entry) error {
	f.appends++
	return errAppend
}

// TestCommitDecisionRecordsTheDecidingActorAndTheBody pins the chain entry an approval writes. It
// is what a change-management review reads to answer who released a change and what exactly they
// released, so the actor, the actor's type, the path, and the committed digest all have to be the
// ones the caller asked for and the body has to verify against the digest.
func TestCommitDecisionRecordsTheDecidingActorAndTheBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Verdict is the decision.
		Verdict string
		// Actor is who decided.
		Actor string
		// ActorType is how they authenticated.
		ActorType string
	}{{ // Test 0: A person approving in a browser.
		Name: "session approval", Verdict: "approved", Actor: "operator-jane",
		ActorType: "session",
	}, { // Test 1: The same person rejecting.
		Name: "session rejection", Verdict: "rejected", Actor: "operator-jane",
		ActorType: "session",
	}, { // Test 2: An AI agent's token, which the chain must not later present as a person's.
		Name: "agent approval", Verdict: "approved", Actor: "agent-deploybot", ActorType: "agent",
	}, { // Test 3: The command line.
		Name: "cli approval", Verdict: "approved", Actor: "admin-token", ActorType: "cli",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			audits := audit.NewMemStore()
			r := decisionRun()
			before := time.Now().Add(-time.Second)

			specDigest, err := CommitDecision(ctx, audits, r, test.Verdict, test.Actor,
				test.ActorType)
			if err != nil {
				t.Fatalf("CommitDecision() error = %v", err)
			}

			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if len(chain) != 1 {
				t.Fatalf("the chain holds %d entries, want exactly the one decision", len(chain))
			}
			e := chain[0]
			if e.Method != audit.MethodDecision {
				t.Errorf("method = %q, want %q so the trail reads a decision apart from a request",
					e.Method, audit.MethodDecision)
			}
			wantPath := "/runs/" + r.ID + "/decision/" + test.Verdict
			if diff := cmp.Diff(wantPath, e.Path); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}
			if e.Actor != test.Actor || e.ActorType != test.ActorType {
				t.Errorf("actor = %q/%q, want %q/%q; a decision an agent made must not later read "+
					"as a person's", e.Actor, e.ActorType, test.Actor, test.ActorType)
			}
			if e.ID == "" || e.Seq != 1 || e.Hash == "" {
				t.Errorf("entry = %+v, want it linked into the chain", e)
			}
			if e.At.Before(before) || e.At.After(time.Now().Add(time.Second)) {
				t.Errorf("entry time = %v, want the moment the decision was recorded", e.At)
			}

			// The committed digest has to be the commitment to the decision body a receipt rebuilds.
			body, wantSpec, err := DecisionBody(r, test.Verdict)
			if err != nil {
				t.Fatalf("DecisionBody() error = %v", err)
			}
			if specDigest != wantSpec {
				t.Errorf("returned spec digest = %q, want %q", specDigest, wantSpec)
			}
			if e.ContentDigest == "" || e.Nonce == "" {
				t.Fatalf("entry carries digest %q and nonce %q, want both", e.ContentDigest, e.Nonce)
			}
			if !audit.VerifyContentDigest(e.ContentDigest, e.Nonce, body) {
				t.Errorf("the decision body does not match the digest the chain committed:\n%s",
					body)
			}
		})
	}
}

// TestCommitDecisionFailsClosedWhenTheChainRefuses proves a decision that cannot be recorded is not
// a decision this system acts on. The caller appends before releasing the run, so an error that
// came back with a usable spec digest, or a partially written entry, would release a change with no
// evidence that anybody approved it.
func TestCommitDecisionFailsClosedWhenTheChainRefuses(t *testing.T) {
	t.Parallel()
	audits := &failingAudits{Store: audit.NewMemStore()}
	specDigest, err := CommitDecision(context.Background(), audits, decisionRun(), "approved",
		"operator-jane", "session")
	if !errors.Is(err, errAppend) {
		t.Fatalf("CommitDecision() error = %v, want the store's refusal reported", err)
	}
	if specDigest != "" {
		t.Errorf("CommitDecision() returned the spec digest %q beside its error, which a caller "+
			"would stamp on the run as though the decision were recorded", specDigest)
	}
	if audits.appends != 1 {
		t.Errorf("the store saw %d appends, want the one attempt", audits.appends)
	}
}

// TestCommitDecisionWritesNothingWhenTheSpecWillNotEncode proves the refusal happens before the
// chain is touched. A chain entry naming a decision whose body could not be built would be evidence
// of an approval nobody can check.
func TestCommitDecisionWritesNothingWhenTheSpecWillNotEncode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	r := decisionRun()
	r.ExtraVars = map[string]any{"ratio": math.Inf(1)}

	specDigest, err := CommitDecision(ctx, audits, r, "approved", "operator-jane", "session")
	if err == nil {
		t.Fatal("CommitDecision() on an unencodable spec = nil error")
	}
	if specDigest != "" {
		t.Errorf("CommitDecision() returned the spec digest %q beside its error", specDigest)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("the chain holds %d entries, want none written for a decision that failed", len(chain))
	}
}

// TestCommitDecisionKeepsUnusualRunIdentifiersOutOfTheChainPathVerbatim pins that the path is built
// from the run id and the verdict as they stand, and that the chain still links whatever text they
// carry. The id and the verdict come from a request, so this is the point where somebody else's
// bytes reach the entry's hashed fields.
func TestCommitDecisionKeepsUnusualRunIdentifiersOutOfTheChainPathVerbatim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// RunID is the run being decided on.
		RunID string
		// Verdict is the decision.
		Verdict string
	}{
		// Test 0: No id at all.
		{Name: "empty id", RunID: "", Verdict: "approved"},
		// Test 1: A path separator inside the id.
		{Name: "slash in id", RunID: "a/b", Verdict: "approved"},
		// Test 2: Multibyte text.
		{Name: "unicode id", RunID: "run_日本", Verdict: "approved"},
		// Test 3: A stray byte, which the chain's own escaping has to normalize before hashing.
		{Name: "invalid utf8", RunID: "run_\xff", Verdict: "approved"},
		// Test 4: A separator in the verdict rather than the id.
		{Name: "verdict with slash", RunID: "run_1", Verdict: "a/b"},
		// Test 5: An id far longer than anything the server mints.
		{Name: "long id", RunID: strings.Repeat("r", 4096), Verdict: "approved"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			audits := audit.NewMemStore()
			r := decisionRun()
			r.ID = test.RunID

			if _, err := CommitDecision(ctx, audits, r, test.Verdict, "op", "session"); err != nil {
				t.Fatalf("CommitDecision() error = %v", err)
			}
			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if len(chain) != 1 {
				t.Fatalf("the chain holds %d entries, want one", len(chain))
			}
			if chain[0].Hash == "" {
				t.Error("the entry was not linked, so the decision is outside the tamper evidence")
			}
			// The stored path is normalized for hashing, so it names the run rather than matching
			// it byte for byte, but it must still carry the decision and the verdict.
			if !strings.HasPrefix(chain[0].Path, "/runs/") ||
				!strings.Contains(chain[0].Path, "/decision/") {
				t.Errorf("path = %q, want it to name the run's decision", chain[0].Path)
			}
		})
	}
}

// TestCommitDecisionIsSafeUnderConcurrentApprovals proves several approvals landing at once each
// get their own linked entry with a distinct sequence. Two approvers acting in the same second is
// ordinary, and a chain that dropped or collided an entry under that would lose the record of a
// decision that released a change.
func TestCommitDecisionIsSafeUnderConcurrentApprovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	const deciders = 24

	var wg sync.WaitGroup
	errs := make(chan error, deciders)
	for i := 0; i < deciders; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := decisionRun()
			r.ID = fmt.Sprintf("run_%02d", n)
			if _, err := CommitDecision(ctx, audits, r, "approved",
				fmt.Sprintf("operator-%02d", n), "session"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("CommitDecision() error = %v", err)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != deciders {
		t.Fatalf("the chain holds %d entries, want one per decision (%d)", len(chain), deciders)
	}
	seen := make(map[int64]bool, deciders)
	for i, e := range chain {
		if seen[e.Seq] {
			t.Errorf("sequence %d appears twice", e.Seq)
		}
		seen[e.Seq] = true
		if i > 0 && e.PrevHash != chain[i-1].Hash {
			t.Errorf("entry %d does not link to its predecessor, so the chain is broken", i)
		}
	}
}

// TestParseRoundTripsAnOutcomeRecord proves the disclosed body a verifier reads decodes back into
// the record it was built from. The receipt path hands somebody these bytes and tells them what the
// run did, so a field that did not survive the trip is a claim the evidence cannot support.
func TestParseRoundTripsAnOutcomeRecord(t *testing.T) {
	t.Parallel()
	exit := 2
	started := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	idx := 1
	want := Record{
		RunID: "run_1", Status: "failed", ExitCode: &exit, Tool: "ansible", Playbook: "site.yml",
		Inventory: "prod", Image: "registry/ansible:1", StartedAt: &started, EndedAt: &ended,
		LogSHA256: "abc", SpecDigest: "sha256:def", CommitSHA: "cafe", DryRun: true,
		PolicySet: &run.PolicySet{Digest: "sha256:pol", Count: 2, Rules: []string{"a", "b"}},
		Hosts: []RecordHost{{Host: "web01", Worst: "failed", OK: 1, Changed: 2, Failures: 3,
			Unreachable: 4, Skipped: 5}},
		Tasks: []RecordTask{{Task: "deploy", Milliseconds: 14}},
		Children: []RecordChild{{RunID: "run_2", Name: "build", Index: &idx, Attempt: 1,
			Status: "succeeded", ExitCode: &exit, LogSHA256: "beef"}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Parse() round trip mismatch (-want +got):\n%s", diff)
	}
}

// TestParseRefusesABodyThatIsNotTheRecord proves a verifier is told the body did not decode rather
// than being handed a zero record it would read as a run that did nothing.
func TestParseRefusesABodyThatIsNotTheRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the body.
		Name string
		// In is the body handed to the verifier.
		In []byte
	}{
		{Name: "empty", In: nil},                          // Test 0: No body at all.
		{Name: "truncated", In: []byte(`{"run_id":`)},     // Test 1: Cut off mid-object.
		{Name: "array", In: []byte(`[]`)},                 // Test 2: Not an object.
		{Name: "wrong type", In: []byte(`{"run_id":42}`)}, // Test 3: A field of the wrong type.
		{Name: "garbage", In: []byte("\x00binary")},       // Test 4: Not JSON at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(test.In); err == nil {
				t.Errorf("Parse(%q) = nil error, want the undecodable body reported", test.In)
			}
		})
	}
}
