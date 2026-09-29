package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// richRun returns a run with every pointer, slice, and map field populated, so a deep-copy check
// has something to break on each one.
func richRun(t *testing.T) *Run {
	t.Helper()
	code := 3
	shardIdx, shardCount, stepIdx := 1, 4, 2
	parent := "run_parent"
	retryOf := "run_origin"
	started := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	claimed := started.Add(time.Second)
	return &Run{
		ID: "run_rich", Playbook: "site.yml", Inventory: "hosts.ini",
		Tool: ToolAnsible, Status: StatusFailed, ExitCode: &code,
		CreatedAt: started, StartedAt: &started, EndedAt: &ended,
		ParentID: &parent, ShardIndex: &shardIdx, ShardCount: &shardCount,
		StepIndex: &stepIdx, RetryOf: &retryOf, ClaimedAt: &claimed,
		Tags: []string{"deploy"}, SkipTags: []string{"slow"},
		CredentialIDs: []string{"cred_a"},
		ExtraVars:     map[string]any{"env": "prod"},
		Outputs:       map[string]any{"version": "1.2.3"},
		Labels:        map[string]string{"team": "core"},
		Notifications: []NotifyTarget{{Kind: NotifySlack, URL: "https://hooks"}},
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml"},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
		},
		PolicySet: &PolicySet{Digest: "d1", Count: 2, Rules: []string{"rule a", "rule b"}},
	}
}

// TestCloneReturnsAnEqualRun pins that a clone is equal to what it came from, which is the half of
// the contract every caller relies on before it relies on the isolation half.
func TestCloneReturnsAnEqualRun(t *testing.T) {
	t.Parallel()
	orig := richRun(t)
	if diff := cmp.Diff(orig, orig.Clone(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("clone is not equal to the original (-want +got):\n%s", diff)
	}
	if (*Run)(nil).Clone() != nil {
		t.Error("cloning a nil run returned something, want nil")
	}
}

// TestCloneIsolatesEveryPointerField pins that writing through a clone's pointer fields leaves the
// original alone.
//
// The store hands a clone to every reader, so a shared pointer here is a reader able to rewrite
// stored state without going through a write path. That is not hypothetical for the exit code and
// the timestamps: the run detail page, the dossier, and the metrics scrape all hold one of these.
func TestCloneIsolatesEveryPointerField(t *testing.T) {
	t.Parallel()
	orig := richRun(t)
	clone := orig.Clone()

	*clone.ExitCode = 99
	*clone.StartedAt = time.Unix(0, 0).UTC()
	*clone.EndedAt = time.Unix(0, 0).UTC()
	*clone.ClaimedAt = time.Unix(0, 0).UTC()
	*clone.ParentID = "run_other"
	*clone.ShardIndex = 42
	*clone.ShardCount = 42
	*clone.StepIndex = 42
	*clone.RetryOf = "run_other"

	want := richRun(t)
	if diff := cmp.Diff(want, orig, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("writing through the clone changed the original (-want +got):\n%s", diff)
	}
}

// TestCloneIsolatesEveryCollectionField pins that a clone's slices and maps are its own.
//
// Credential ids decide which secrets a run is handed, extra vars are spliced into what it runs,
// and a pipeline step's dependency list decides the order things happen in. A store hands a clone
// to every reader, so a shared backing array means an ordinary reader can change what a stored run
// will do, with no write path involved and nothing in the audit chain to show it.
//
// The tag lists are checked separately, in TestAReaderCannotRewriteAStoredRunsTags, because they
// are not copied today.
func TestCloneIsolatesEveryCollectionField(t *testing.T) {
	t.Parallel()
	orig := richRun(t)
	clone := orig.Clone()

	clone.CredentialIDs[0] = "cred_other"
	clone.ExtraVars["env"] = "staging"
	clone.Outputs["version"] = "0.0.0"
	clone.Labels["team"] = "other"
	clone.Notifications[0].URL = "https://attacker"
	clone.Steps[0].Name = "renamed"
	clone.Steps[1].DependsOn[0] = "nothing"

	want := richRun(t)
	if diff := cmp.Diff(want, orig, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("writing through the clone changed the original (-want +got):\n%s", diff)
	}
}

// TestCloneIsolatesTheRecordedPolicySet pins that a clone's PolicySet is its own.
//
// The policy set is evidence. It is recorded on the run rather than referenced precisely so the
// record reads without asking a server what a digest meant, and "which rules were in force when
// this change was submitted" is the question a change-management review asks. A shared pointer
// makes it rewritable by anyone holding a run read.
func TestCloneIsolatesTheRecordedPolicySet(t *testing.T) {
	t.Parallel()
	orig := richRun(t)
	clone := orig.Clone()

	clone.PolicySet.Digest = "forged"
	clone.PolicySet.Count = 0
	clone.PolicySet.Rules[0] = "there were no rules"

	want := richRun(t)
	if diff := cmp.Diff(want.PolicySet, orig.PolicySet, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("writing through the clone rewrote the recorded policy set (-want +got):\n%s", diff)
	}
}

// TestAReaderCannotRewriteAStoredRunsTags is the same isolation question asked where it bites: a
// caller that reads a run out of the store, edits the value it was handed, and reads again.
//
// Get, List, Shards, Steps, NonTerminal, and Claim all return Clone's output, and the store's own
// copy came from Clone too, so both sides of the store boundary point at one backing array. A page
// handler, an export, or a plugin that edits the run it was given is silently editing the run the
// executor will run.
func TestAReaderCannotRewriteAStoredRunsTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	stored := &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusPending, CreatedAt: time.Now(),
		Tags: []string{"deploy"}, SkipTags: []string{"slow"},
	}
	if err := store.Save(ctx, stored); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	first, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	first.Tags[0] = "destroy"
	first.SkipTags[0] = "none"

	again, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff([]string{"deploy"}, again.Tags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a reader rewrote the stored run's tags (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"slow"}, again.SkipTags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a reader rewrote the stored run's skip tags (-want +got):\n%s", diff)
	}
}

// TestTheClaimSecretNeverSerializes pins that the per-claim capability stays out of every JSON
// representation of a run.
//
// The json:"-" tag on it is load-bearing and stated to be: every worker on the relay presents the
// same shared token, so the per-claim secret is what proves a report came from the worker that
// holds the lease. Serializing it would put it in the relay's own run reads, every bundle, every
// SIEM forward, and every evidence document, letting a worker read back a capability it was never
// issued. The idempotency key is a server-side control field for the same reason: a caller who can
// read another submission's key can collide with it deliberately.
func TestTheClaimSecretNeverSerializes(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(&Run{
		ID: "run_1", ClaimSecret: "c1aim53cr3tva1u3", IdempotencyKey: "k3yva1u3",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, leaked := range []string{"c1aim53cr3tva1u3", "claim_secret", "k3yva1u3", "idempotency"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("the serialized run carries %q\ngot: %s", leaked, body)
		}
	}

	// And a round trip through JSON cannot set them either, so a caller cannot plant a capability
	// by sending one on a submission body.
	var back Run
	if err := json.Unmarshal([]byte(
		`{"id":"run_1","claim_secret":"planted","idempotency_key":"planted"}`), &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if back.ClaimSecret != "" {
		t.Errorf("ClaimSecret = %q, want empty: a request body set a per-claim capability",
			back.ClaimSecret)
	}
	if back.IdempotencyKey != "" {
		t.Errorf("IdempotencyKey = %q, want empty: a request body set a server control field",
			back.IdempotencyKey)
	}
}

// TestNewClaimSecretIsUnpredictable pins that each claim gets its own full-width capability, since
// a worker that could guess another claim's secret could report outcomes for a run it does not
// hold.
func TestNewClaimSecretIsUnpredictable(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 256)
	for range 256 {
		s := NewClaimSecret()
		if len(s) != 64 {
			t.Fatalf("len(NewClaimSecret()) = %d, want 64 hex characters for 256 bits", len(s))
		}
		if strings.Trim(s, "0123456789abcdef") != "" {
			t.Fatalf("NewClaimSecret() = %q, want lowercase hex", s)
		}
		if seen[s] {
			t.Fatalf("NewClaimSecret() repeated %q", s)
		}
		seen[s] = true
	}
}

// TestNewIDsAreDistinctAcrossManyMints pins that run identifiers do not collide in bulk, since a
// repeat would have one run's Save silently replace another's record.
func TestNewIDsAreDistinctAcrossManyMints(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 4096)
	for range 4096 {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}
