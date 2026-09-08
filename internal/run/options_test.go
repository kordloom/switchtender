package run

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestSubmitOptionsWriteExactlyTheirOwnField pins every submit option against the whole run it
// produces rather than against the one field it is named for.
//
// An option that writes a neighboring field passes any assertion that only reads the field the
// option is named after, and the run these build is the execution spec: the tool, the command, the
// dry-run flag, and the approval hold. A constructor that installed the wrong one would submit a
// different change than the caller asked for, and nothing else in the package would notice.
//
//nolint:funlen // Test function.
func TestSubmitOptionsWriteExactlyTheirOwnField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Opt     SubmitOption
		WantRun Run
	}{{ // Test 0: The holding rule is recorded as it was named at the hold.
		Name: "held by policy", Opt: WithHeldByPolicy("prod destroy gate"),
		WantRun: Run{HeldByPolicy: "prod destroy gate"},
	}, { // Test 1: A second pair of eyes is required only when the rule asked for it.
		Name: "require distinct approver", Opt: WithRequireDistinctApprover(true),
		WantRun: Run{RequireDistinctApprover: true},
	}, { // Test 2: Not requiring it leaves the run alone rather than writing false over a true.
		Name: "require distinct approver false", Opt: WithRequireDistinctApprover(false),
		WantRun: Run{},
	}, { // Test 3: The chain entry that authorized the creation.
		Name: "audit receipt", Opt: WithAuditReceiptOf("41:9f2caa"),
		WantRun: Run{AuditReceipt: "41:9f2caa"},
	}, { // Test 4: Stored credentials materialized for the run.
		Name: "credential ids", Opt: WithCredentialIDs([]string{"cred_a", "cred_b"}),
		WantRun: Run{CredentialIDs: []string{"cred_a", "cred_b"}},
	}, { // Test 5: The git project the paths resolve inside.
		Name: "project", Opt: WithProject("proj_1"), WantRun: Run{ProjectID: "proj_1"},
	}, { // Test 6: A stored inventory instead of a file path.
		Name: "inventory", Opt: WithInventory("inv_1"), WantRun: Run{InventoryID: "inv_1"},
	}, { // Test 7: The owning organization that scopes an objectless run.
		Name: "org id", Opt: WithOrgID("org_acme"), WantRun: Run{OrgID: "org_acme"},
	}, { // Test 8: An empty org leaves the run unowned rather than stamping an empty owner.
		Name: "org id empty", Opt: WithOrgID(""), WantRun: Run{},
	}, { // Test 9: The queue restricting which workers may take the run.
		Name: "queue", Opt: WithQueue("gpu"), WantRun: Run{Queue: "gpu"},
	}, { // Test 10: An image and its pull credential travel together.
		Name: "image", Opt: WithImage("ghcr.io/x:1", "cred_pull"),
		WantRun: Run{Image: "ghcr.io/x:1", PullCredentialID: "cred_pull"},
	}, { // Test 11: Injected variables.
		Name: "extra vars", Opt: WithExtraVars(map[string]any{"env": "prod"}),
		WantRun: Run{ExtraVars: map[string]any{"env": "prod"}},
	}, { // Test 12: An empty variable map is a no-op, so it cannot erase vars set earlier.
		Name: "extra vars empty", Opt: WithExtraVars(map[string]any{}), WantRun: Run{},
	}, { // Test 13: The execution tool.
		Name: "tool", Opt: WithTool(ToolBash), WantRun: Run{Tool: ToolBash},
	}, { // Test 14: The tool's primary input.
		Name: "command", Opt: WithCommand("echo hi"), WantRun: Run{Command: "echo hi"},
	}, { // Test 15: The no-change mode.
		Name: "dry run", Opt: WithDryRun(true), WantRun: Run{DryRun: true},
	}, { // Test 16: Requiring approval is what holds a run out of the claim loop.
		Name: "require approval", Opt: WithRequireApproval(true),
		WantRun: Run{Status: StatusPendingApproval},
	}, { // Test 17: Not requiring it leaves the status alone.
		Name: "require approval false", Opt: WithRequireApproval(false), WantRun: Run{},
	}, { // Test 18: What fired the run and which object did it.
		Name: "source", Opt: WithSource("template", "tpl_1"),
		WantRun: Run{Source: "template", SourceID: "tpl_1"},
	}, { // Test 19: The credential name of the person who fired it.
		Name: "actor", Opt: WithActor("alice-token"), WantRun: Run{Actor: "alice-token"},
	}, { // Test 20: The rule set in force at submit, rendered rather than referenced.
		Name: "policy set", Opt: WithPolicySet("d1", 2, []string{"a", "b"}),
		WantRun: Run{PolicySet: &PolicySet{Digest: "d1", Count: 2, Rules: []string{"a", "b"}}},
	}, { // Test 21: The one commit the run may execute.
		Name: "pinned commit", Opt: WithPinnedCommit("deadbeef"),
		WantRun: Run{PinnedCommit: "deadbeef"},
	}, { // Test 22: The account behind the credential, which is what separation of duties compares.
		Name: "actor account", Opt: WithActorAccount("user_7"), WantRun: Run{ActorUserID: "user_7"},
	}, { // Test 23: How the actor authenticated, which is what lets a policy single out an agent.
		Name: "actor type", Opt: WithActorType("agent"), WantRun: Run{ActorType: "agent"},
	}, { // Test 24: The finished run whose spec this one replays.
		Name: "rerun of", Opt: WithRerunOf("run_old"), WantRun: Run{RerunOf: "run_old"},
	}, { // Test 25: User-supplied slicing labels.
		Name: "labels", Opt: WithLabels(map[string]string{"env": "prod"}),
		WantRun: Run{Labels: map[string]string{"env": "prod"}},
	}, { // Test 26: An empty label map is a no-op.
		Name: "labels empty", Opt: WithLabels(nil), WantRun: Run{},
	}, { // Test 27: The host pattern a reconcile targets.
		Name: "limit", Opt: WithLimit("web01"), WantRun: Run{Limit: "web01"},
	}, { // Test 28: Tags narrow which plays run.
		Name: "tags", Opt: WithTags("deploy", "config"),
		WantRun: Run{Tags: []string{"deploy", "config"}},
	}, { // Test 29: Skip tags are a separate list and must not land in Tags.
		Name: "skip tags", Opt: WithSkipTags("slow"), WantRun: Run{SkipTags: []string{"slow"}},
	}, { // Test 30: Verbosity inside the range is kept as given.
		Name: "verbosity", Opt: WithVerbosity(3), WantRun: Run{Verbosity: 3},
	}, { // Test 31: Forks above zero is kept.
		Name: "forks", Opt: WithForks(12), WantRun: Run{Forks: 12},
	}, { // Test 32: Diff mode.
		Name: "diff mode", Opt: WithDiffMode(true), WantRun: Run{DiffMode: true},
	}, { // Test 33: The drift check a reconcile proposal was built from.
		Name: "proposed from", Opt: WithProposedFrom("run_check"),
		WantRun: Run{ProposedFrom: "run_check"},
	}, { // Test 34: The plain-language request an AI turned into the run.
		Name: "intent", Opt: WithIntent("restart the web tier"),
		WantRun: Run{Intent: "restart the web tier"},
	}, { // Test 35: The run a retry was derived from.
		Name: "retry of", Opt: WithRetryOf("run_failed"), WantRun: Run{RetryOf: strPtr("run_failed")},
	}, { // Test 36: An empty source leaves RetryOf nil rather than pointing at an empty id.
		Name: "retry of empty", Opt: WithRetryOf(""), WantRun: Run{},
	}, { // Test 37: The dedupe key, which is a control field and not part of the run's JSON.
		Name: "idempotency key", Opt: WithIdempotencyKey("st:rerun:run_a:1"),
		WantRun: Run{IdempotencyKey: "st:rerun:run_a:1"},
	}, { // Test 38: The execution cap in seconds.
		Name: "timeout", Opt: WithTimeout(600), WantRun: Run{Timeout: 600},
	}, { // Test 39: Per-run notification targets.
		Name: "notifications",
		Opt:  WithNotifications([]NotifyTarget{{Kind: NotifySlack, URL: "https://h"}}),
		WantRun: Run{
			Notifications: []NotifyTarget{{Kind: NotifySlack, URL: "https://h"}},
		},
	}, { // Test 40: An empty target list is a no-op rather than an empty slice on the run.
		Name: "notifications empty", Opt: WithNotifications(nil), WantRun: Run{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var got Run
			test.Opt(&got)
			if diff := cmp.Diff(test.WantRun, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("option wrote the wrong fields (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWithVerbosityClampsToAnsibleRange pins that the verbosity option holds its value inside the
// zero to four range Ansible accepts, since the number is spliced onto a command line as a run of
// v flags and a caller-supplied one comes straight off a JSON body.
func TestWithVerbosityClampsToAnsibleRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        int
		WantLevel int
	}{
		{In: -1, WantLevel: 0},       // Test 0: Below the floor clamps up.
		{In: -1 << 40, WantLevel: 0}, // Test 1: A far negative clamps up, not down.
		{In: 0, WantLevel: 0},        // Test 2: The floor itself.
		{In: 4, WantLevel: 4},        // Test 3: The ceiling itself.
		{In: 5, WantLevel: 4},        // Test 4: One past the ceiling clamps down.
		{In: 1 << 40, WantLevel: 4},  // Test 5: A far positive clamps down.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got Run
			WithVerbosity(test.In)(&got)
			if got.Verbosity != test.WantLevel {
				t.Errorf("WithVerbosity(%d) set %d, want %d", test.In, got.Verbosity, test.WantLevel)
			}
		})
	}
}

// TestZeroAndNegativeOptionsLeaveTheDefault pins that the three options carrying a "leave the
// default" value do exactly that instead of writing a zero or a negative onto the run.
//
// Forks and Timeout are both spliced into how the run executes: a zero fork count would tell
// Ansible to address no hosts, and a zero or negative timeout written over a configured default
// would either kill a run immediately or remove its cap, depending on who reads it. The options say
// a value at or below zero leaves the dispatcher's default, so a caller sending nothing gets the
// default rather than a broken bound.
func TestZeroAndNegativeOptionsLeaveTheDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Opt         SubmitOption
		Start       Run
		WantForks   int
		WantTimeout int
	}{{ // Test 0: Zero forks leaves whatever was already set.
		Name: "forks zero", Opt: WithForks(0), Start: Run{Forks: 8}, WantForks: 8,
	}, { // Test 1: Negative forks leaves whatever was already set.
		Name: "forks negative", Opt: WithForks(-3), Start: Run{Forks: 8}, WantForks: 8,
	}, { // Test 2: Zero timeout leaves whatever was already set.
		Name: "timeout zero", Opt: WithTimeout(0), Start: Run{Timeout: 300}, WantTimeout: 300,
	}, { // Test 3: Negative timeout leaves whatever was already set.
		Name: "timeout negative", Opt: WithTimeout(-1), Start: Run{Timeout: 300}, WantTimeout: 300,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := test.Start
			test.Opt(&got)
			if got.Forks != test.WantForks {
				t.Errorf("forks = %d, want %d", got.Forks, test.WantForks)
			}
			if got.Timeout != test.WantTimeout {
				t.Errorf("timeout = %d, want %d", got.Timeout, test.WantTimeout)
			}
		})
	}
}

// TestTagOptionsDropBlankEntries pins that a blank tag never reaches a stored run.
//
// An empty string in the tag list is not harmless: Ansible reads a tag list positionally, so a
// blank entry silently widens or narrows which plays a run touches compared to what the caller
// asked for, and the caller cannot see it in the stored run.
func TestTagOptionsDropBlankEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       []string
		WantTags []string
	}{
		// Test 0: Nil stays nil.
		{Name: "no tags", In: nil, WantTags: nil},
		// Test 1: A list of nothing but blanks is nil.
		{Name: "only a blank", In: []string{""}, WantTags: nil},
		// Test 2: Still nil.
		{Name: "all blanks", In: []string{"", "", ""}, WantTags: nil},
		// Test 3: A leading blank is dropped.
		{Name: "leading blank", In: []string{"", "a"}, WantTags: []string{"a"}},
		// Test 4: A trailing blank is dropped.
		{Name: "trailing blank", In: []string{"a", ""}, WantTags: []string{"a"}},
		{ // Test 5: A blank between two real tags is dropped without disturbing the order.
			Name: "interior blank", In: []string{"a", "", "b"}, WantTags: []string{"a", "b"},
		},
		{ // Test 6: Unicode and whitespace-only tags are real strings and are kept as sent.
			Name: "unicode kept", In: []string{"配置", " "}, WantTags: []string{"配置", " "},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var tagged, skipped Run
			WithTags(test.In...)(&tagged)
			WithSkipTags(test.In...)(&skipped)
			if diff := cmp.Diff(test.WantTags, tagged.Tags, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Tags mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantTags, skipped.SkipTags, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SkipTags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOptionsCopyTheCallerSlicesAndMaps pins that an option stores a copy rather than the caller's
// own slice or map.
//
// A caller that reuses one buffer for several submissions, which is what a loop over a template's
// runs looks like, would otherwise have every run it created point at the same tags, credentials,
// variables, and labels, and the last write would decide what all of them execute.
func TestOptionsCopyTheCallerSlicesAndMaps(t *testing.T) {
	t.Parallel()
	tags := []string{"deploy"}
	skip := []string{"slow"}
	creds := []string{"cred_a"}
	vars := map[string]any{"env": "prod"}
	labels := map[string]string{"team": "core"}
	targets := []NotifyTarget{{Kind: NotifySlack, URL: "https://h"}}

	var r Run
	ApplyOptions(&r, []SubmitOption{
		WithTags(tags...), WithSkipTags(skip...), WithCredentialIDs(creds),
		WithExtraVars(vars), WithLabels(labels), WithNotifications(targets),
	})

	tags[0] = "destroy"
	skip[0] = "none"
	creds[0] = "cred_other"
	vars["env"] = "staging"
	labels["team"] = "other"
	targets[0].URL = "https://attacker"

	want := Run{
		Tags: []string{"deploy"}, SkipTags: []string{"slow"}, CredentialIDs: []string{"cred_a"},
		ExtraVars: map[string]any{"env": "prod"}, Labels: map[string]string{"team": "core"},
		Notifications: []NotifyTarget{{Kind: NotifySlack, URL: "https://h"}},
	}
	if diff := cmp.Diff(want, r, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the run changed when the caller reused its buffers (-want +got):\n%s", diff)
	}
}

// TestApplyOptionsAppliesInOrder pins that later options win over earlier ones, which is what lets
// a caller build a base list and override one entry rather than having to rebuild the list.
func TestApplyOptionsAppliesInOrder(t *testing.T) {
	t.Parallel()
	var r Run
	ApplyOptions(&r, []SubmitOption{
		WithTool(ToolAnsible), WithCommand("first"), WithTool(ToolBash), WithCommand("second"),
	})
	if r.Tool != ToolBash || r.Command != "second" {
		t.Errorf("tool/command = %q/%q, want the later options to win", r.Tool, r.Command)
	}
	// An empty option list changes nothing, and a nil list is not a panic.
	before := r
	ApplyOptions(&r, nil)
	if diff := cmp.Diff(before, r, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("applying no options changed the run (-want +got):\n%s", diff)
	}
}

// strPtr returns a pointer to s, for building expected runs that carry optional string fields.
func strPtr(s string) *string { return &s }
