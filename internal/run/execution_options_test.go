package run

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// fullSpecRun returns a run carrying every field ExecutionOptions is meant to carry forward, plus
// the per-run fields it is meant to leave behind, so one derivation exercises both halves.
func fullSpecRun(t *testing.T) *Run {
	t.Helper()
	shards := 4
	return &Run{
		ID:               "run_parent",
		Playbook:         "site.yml",
		Inventory:        "hosts.ini",
		Tool:             ToolBash,
		Command:          "echo hello",
		DryRun:           true,
		Status:           StatusRunning,
		ExtraVars:        map[string]any{"env": "prod"},
		CredentialIDs:    []string{"cred_a", "cred_b"},
		Tags:             []string{"deploy"},
		SkipTags:         []string{"slow"},
		Verbosity:        3,
		Forks:            25,
		DiffMode:         true,
		ProjectID:        "proj_1",
		InventoryID:      "inv_1",
		Queue:            "gpu",
		Timeout:          900,
		Image:            "ghcr.io/org/img:1",
		PullCredentialID: "cred_pull",
		PinnedCommit:     "deadbeefcafe",
		// Everything below belongs to this one run rather than to the spec.
		Limit:         "shard-0-hosts",
		Labels:        map[string]string{"team": "core"},
		Notifications: []NotifyTarget{{Kind: NotifySlack, URL: "https://hooks"}},
		Source:        "template",
		SourceID:      "tpl_1",
		Actor:         "alice",
		ActorUserID:   "user_7",
		ActorType:     "session",
		OrgID:         "org_acme",
		HeldByPolicy:  "prod gate",
		AuditReceipt:  "41:9f2caa",
		ShardCount:    &shards,
	}
}

// TestExecutionOptionsCarriesTheWholeSpec pins that deriving a run from another run keeps every
// field that decides how it executes.
//
// This is the one description of a run's execution spec, and every path that used to write its own
// list lost a field: a shard dropped the parent's extra vars, image, and timeout, a pipeline step
// dropped its own set, and a rerun dropped the timeout. A dry-run-only spec that loses its flag
// makes real changes on real hosts, so a field silently missing here is not cosmetic. Comparing the
// derived run field by field against what the source carried is what catches the next dropped one.
func TestExecutionOptionsCarriesTheWholeSpec(t *testing.T) {
	t.Parallel()
	src := fullSpecRun(t)

	derived := &Run{ID: "run_child", Status: StatusPending}
	ApplyOptions(derived, src.ExecutionOptions())

	want := &Run{
		ID: "run_child", Status: StatusPending,
		Tool: ToolBash, Command: "echo hello", DryRun: true,
		ExtraVars: map[string]any{"env": "prod"}, CredentialIDs: []string{"cred_a", "cred_b"},
		Tags: []string{"deploy"}, SkipTags: []string{"slow"},
		Verbosity: 3, Forks: 25, DiffMode: true,
		ProjectID: "proj_1", InventoryID: "inv_1", Queue: "gpu", Timeout: 900,
		Image: "ghcr.io/org/img:1", PullCredentialID: "cred_pull",
		PinnedCommit: "deadbeefcafe",
	}
	if diff := cmp.Diff(want, derived, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the derived run's spec differs from the source (-want +got):\n%s", diff)
	}
}

// TestExecutionOptionsLeavesPerRunFieldsAlone pins the other half of the contract: the fields the
// options deliberately exclude do not travel.
//
// Limit is the one that matters most. It names a shard's own hosts, so a parent's limit copied onto
// a shard would make every shard target the parent's whole pattern instead of its own slice, which
// runs the change once per shard across the same hosts. Labels, notifications, and provenance are
// excluded because the caller deriving the run decides those for itself, and a copied audit receipt
// would credit a new request to an older one.
func TestExecutionOptionsLeavesPerRunFieldsAlone(t *testing.T) {
	t.Parallel()
	src := fullSpecRun(t)

	derived := &Run{ID: "run_child", Status: StatusPending, Limit: "its-own-hosts"}
	ApplyOptions(derived, src.ExecutionOptions())

	if derived.Limit != "its-own-hosts" {
		t.Errorf("Limit = %q, want the child's own pattern: a parent's limit copied onto a shard "+
			"makes every shard run the whole pattern", derived.Limit)
	}
	tests := []struct {
		Name string
		Got  string
	}{
		{Name: "source", Got: derived.Source},               // Test 0: Provenance is the caller's.
		{Name: "source id", Got: derived.SourceID},          // Test 1: Same.
		{Name: "actor", Got: derived.Actor},                 // Test 2: Same.
		{Name: "actor user id", Got: derived.ActorUserID},   // Test 3: Same.
		{Name: "actor type", Got: derived.ActorType},        // Test 4: Same.
		{Name: "org id", Got: derived.OrgID},                // Test 5: Stamped by the new request.
		{Name: "held by policy", Got: derived.HeldByPolicy}, // Test 6: A new hold names its own rule.
		{Name: "audit receipt", Got: derived.AuditReceipt},  // Test 7: A new request has its own.
		{Name: "playbook", Got: derived.Playbook},           // Test 8: The caller sets the paths.
		{Name: "inventory", Got: derived.Inventory},         // Test 9: Same.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if test.Got != "" {
				t.Errorf("%s = %q, want empty: it belongs to the source run, not to the spec",
					test.Name, test.Got)
			}
		})
	}
	if len(derived.Labels) != 0 {
		t.Errorf("Labels = %v, want none carried", derived.Labels)
	}
	if len(derived.Notifications) != 0 {
		t.Errorf("Notifications = %v, want none carried", derived.Notifications)
	}
	if derived.ShardCount != nil {
		t.Errorf("ShardCount = %v, want nil: the shard shape belongs to the split", *derived.ShardCount)
	}
}

// TestExecutionOptionsOmitsEmptyOptionalFields pins that a spare source run does not stamp empty
// project, inventory, queue, image, or pin values onto the run it derives.
//
// An empty pinned commit written onto a derived run would be harmless, but an empty image or
// project id written over a value the caller had already set would not be: the caller applies these
// options on top of a run it has already shaped, so an unconditional write is a silent erase.
func TestExecutionOptionsOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	spare := &Run{ID: "run_spare", Playbook: "site.yml"}

	derived := &Run{
		ID: "run_child", ProjectID: "proj_child", InventoryID: "inv_child", Queue: "queue_child",
		Timeout: 60, Image: "img_child", PullCredentialID: "cred_child", PinnedCommit: "abc",
	}
	ApplyOptions(derived, spare.ExecutionOptions())

	want := &Run{
		ID: "run_child", ProjectID: "proj_child", InventoryID: "inv_child", Queue: "queue_child",
		Timeout: 60, Image: "img_child", PullCredentialID: "cred_child", PinnedCommit: "abc",
	}
	if diff := cmp.Diff(want, derived, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a spare source erased the child's own fields (-want +got):\n%s", diff)
	}
}

// TestExecutionOptionsDoesNotShareTheSourceCollections pins that a derived run holds its own copies
// of the tag, credential, and variable collections.
//
// A split derives many shards from one parent through this call. If they shared the parent's
// slices, a per-shard adjustment to one shard's tags or credentials would change every other
// shard's, and the change would not be visible in the shard the caller edited.
func TestExecutionOptionsDoesNotShareTheSourceCollections(t *testing.T) {
	t.Parallel()
	src := fullSpecRun(t)

	first := &Run{ID: "run_a"}
	second := &Run{ID: "run_b"}
	ApplyOptions(first, src.ExecutionOptions())
	ApplyOptions(second, src.ExecutionOptions())

	first.Tags[0] = "destroy"
	first.SkipTags[0] = "none"
	first.CredentialIDs[0] = "cred_other"
	first.ExtraVars["env"] = "staging"

	if diff := cmp.Diff([]string{"deploy"}, second.Tags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("two runs derived from one parent share a tag slice (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"slow"}, second.SkipTags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("two runs derived from one parent share a skip-tag slice (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"cred_a", "cred_b"}, second.CredentialIDs,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("two runs derived from one parent share a credential slice (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{"env": "prod"}, second.ExtraVars,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("two runs derived from one parent share a variable map (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"deploy"}, src.Tags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("editing a derived run changed the source it came from (-want +got):\n%s", diff)
	}
}

// TestExecutionOptionsKeepsTheDryRunFlagFalse pins that deriving from a run that is not a dry run
// writes the flag as false rather than leaving whatever the target already held.
//
// The flag is carried unconditionally, in both directions. A rerun of a real apply built on top of
// a run object that happened to carry DryRun would otherwise silently downgrade the apply into a
// check that changes nothing, and the run would report success having done no work.
func TestExecutionOptionsKeepsTheDryRunFlagFalse(t *testing.T) {
	t.Parallel()
	apply := &Run{ID: "run_apply", Tool: ToolTerraform, Command: "/infra", DryRun: false}
	target := &Run{ID: "run_child", DryRun: true}
	ApplyOptions(target, apply.ExecutionOptions())
	if target.DryRun {
		t.Error("a run derived from a real apply stayed a dry run, so it would change nothing")
	}
}
