package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// settingsStore writes a policy file loading one Rego module with the given body under the given
// entry settings, and returns the store serving it with the label its notes and holds carry.
func settingsStore(t *testing.T, body, settings string) (*policy.FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate.rego"),
		[]byte("package switchtender\n\n"+body+"\n"), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	path := filepath.Join(dir, "policies.yml")
	if err := os.WriteFile(path,
		[]byte("rego:\n  - name: rego gate\n    files: [gate.rego]\n"+settings), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	listed, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	return store, "rego sha256:" + listed[0].Rego.Digest()[:12]
}

// noteSettings is the entry setting that makes a policy's warnings notes.
const noteSettings = "    warn: note\n"

// TestARegoNoteIsRecordedOnTheRunWithoutHoldingIt drives decision 3 through the dispatcher. A
// warning under warn: note lands on the run and survives its execution in the store, the run is not
// held for it, a hold rule in the same bundle still holds, and the same bundle under the default
// holds the run on the warning instead of noting it.
func TestARegoNoteIsRecordedOnTheRunWithoutHoldingIt(t *testing.T) {
	t.Parallel()
	body := `warn contains "no change ticket on the run" if not input.run.labels.ticket

hold contains "python needs a person" if input.run.tool == "python"`
	tests := []struct {
		// Settings are the policy entry's settings.
		Settings string
		// Opts describe the submitted run.
		Opts []run.SubmitOption
		// WantHeldBy is the rule the run is held by, empty when it is not held.
		WantHeldBy string
		// WantNotes are the notes the run carries, before the bundle digest.
		WantNotes []string
	}{{ // Test 0: A noted warning: recorded, and the run goes ahead.
		Settings:  noteSettings,
		Opts:      []run.SubmitOption{run.WithTool("bash"), run.WithCommand("uptime")},
		WantNotes: []string{"rego gate (no change ticket on the run, "},
	}, { // Test 1: A run the warning does not fire on carries nothing.
		Settings: noteSettings,
		Opts: []run.SubmitOption{run.WithTool("bash"), run.WithCommand("uptime"),
			run.WithLabels(map[string]string{"ticket": "CHG-7"})},
	}, { // Test 2: The bundle's hold rule still holds, and the note rides beside the hold.
		Settings:   noteSettings,
		Opts:       []run.SubmitOption{run.WithTool("python"), run.WithCommand("print('deploy')")},
		WantHeldBy: "rego gate (python needs a person, ",
		WantNotes:  []string{"rego gate (no change ticket on the run, "},
	}, { // Test 3: Under the default the same warning holds, and nothing is noted.
		Settings:   "",
		Opts:       []run.SubmitOption{run.WithTool("bash"), run.WithCommand("uptime")},
		WantHeldBy: "rego gate (no change ticket on the run, ",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies, bundle := settingsStore(t, body, test.Settings)
			store := run.NewMemStore()
			d := New(store, &fakeRunnerLister{hosts: []string{"a"}}, nil, WithPolicies(policies))
			t.Cleanup(d.Close)
			created, err := d.Submit(context.Background(), "", "", test.Opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			wantNotes := make([]string, 0, len(test.WantNotes))
			for _, n := range test.WantNotes {
				wantNotes = append(wantNotes, n+bundle+")")
			}
			if diff := cmp.Diff(wantNotes, created.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("notes on the submitted run mismatch (-want +got):\n%s", diff)
			}
			if test.WantHeldBy == "" {
				if created.Status == run.StatusPendingApproval {
					t.Fatalf("the run was held by %q, want it to go ahead", created.HeldByPolicy)
				}
				// The note is part of the run's record, so it has to outlast the execution.
				done := waitTerminal(t, store, created.ID)
				if diff := cmp.Diff(wantNotes, done.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("notes on the finished run mismatch (-want +got):\n%s", diff)
				}
				return
			}
			if created.Status != run.StatusPendingApproval {
				t.Fatalf("status = %q, want held by %q", created.Status, test.WantHeldBy)
			}
			if want := test.WantHeldBy + bundle + ")"; created.HeldByPolicy != want {
				t.Errorf("held by %q, want %q", created.HeldByPolicy, want)
			}
		})
	}
}

// TestAPolicyNeverWritesTheRunsOwnSecretIntoItsRecord pins the masking on what a policy writes onto
// a run. A module can quote input.run.command, and a script can carry a token inline. The note is
// committed with the run's outcome and disclosed by every receipt, and the hold label is printed in
// the dossier, so the secret has to be gone before either is recorded.
func TestAPolicyNeverWritesTheRunsOwnSecretIntoItsRecord(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token-value-91"
	body := `warn contains msg if {
	input.run.tool == "bash"
	msg := sprintf("script reads %s", [input.run.command])
}`
	tests := []struct {
		// Settings are the policy entry's settings.
		Settings string
		// Text reads the policy's text off the submitted run.
		Text func(r *run.Run) string
	}{{ // Test 0: A note.
		Settings: noteSettings,
		Text:     func(r *run.Run) string { return strings.Join(r.PolicyNotes, "\n") },
	}, { // Test 1: A hold.
		Settings: "",
		Text:     func(r *run.Run) string { return r.HeldByPolicy },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies, _ := settingsStore(t, body, test.Settings)
			d := New(run.NewMemStore(), &fakeRunnerLister{hosts: []string{"a"}}, nil,
				WithPolicies(policies), WithNoJanitor())
			t.Cleanup(d.Close)
			created, err := d.Submit(context.Background(), "", "", run.WithTool("bash"),
				run.WithCommand("API_TOKEN="+secret+" ./deploy.sh"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := test.Text(created)
			if !strings.Contains(got, "script reads") {
				t.Fatalf("the policy's text was not recorded at all: %q", got)
			}
			if strings.Contains(got, secret) {
				t.Errorf("the run's record carries its secret in plain text: %q", got)
			}
			if !strings.Contains(got, maskToken) {
				t.Errorf("the recorded text %q does not show where the secret was masked", got)
			}
		})
	}
}

// TestARefusalAndAPreviewNeverQuoteTheRunsOwnSecret pins the masking on the policy text a caller
// reads rather than a record holds: the error a refused submission answers with, a refused
// workflow's, and the rule a pull request's preview of its apply names in the comment. A Rego
// verdict is labeled with the messages its module built, and a module can quote input.run.command.
func TestARefusalAndAPreviewNeverQuoteTheRunsOwnSecret(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token-value-92"
	const command = "API_TOKEN=" + secret + " ./deploy.sh"
	deny := `deny contains msg if {
	input.run.command != ""
	msg := sprintf("script reads %s", [input.run.command])
}`
	hold := `hold contains msg if {
	input.run.command != ""
	msg := sprintf("script reads %s", [input.run.command])
}`
	tests := []struct {
		// Body is the policy module.
		Body string
		// Text returns the policy text the caller reads.
		Text func(t *testing.T, d *Dispatcher) string
	}{{ // Test 0: A refused submission.
		Body: deny,
		Text: func(t *testing.T, d *Dispatcher) string {
			_, err := d.Submit(context.Background(), "", "", run.WithTool("bash"),
				run.WithCommand(command))
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("Submit() error = %v, want a refusal", err)
			}
			return err.Error()
		},
	}, { // Test 1: A refused workflow step.
		Body: deny,
		Text: func(t *testing.T, d *Dispatcher) string {
			_, err := d.SubmitPipeline(context.Background(), "nightly", "",
				[]run.PipelineStep{{Name: "ship", Tool: "bash", Command: command}})
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("SubmitPipeline() error = %v, want a refusal", err)
			}
			return err.Error()
		},
	}, { // Test 2: A pull request's preview of an apply the rule refuses.
		Body: deny,
		Text: func(t *testing.T, d *Dispatcher) string {
			preview, err := d.PreviewApply(context.Background(),
				&run.Run{ID: "run_apply", Tool: "bash", Command: command}, 0, true)
			if err != nil || preview.Outcome != ApplyDenied {
				t.Fatalf("PreviewApply() = %+v, %v, want a refusal", preview, err)
			}
			return preview.Rule
		},
	}, { // Test 3: A pull request's preview of an apply the rule holds.
		Body: hold,
		Text: func(t *testing.T, d *Dispatcher) string {
			preview, err := d.PreviewApply(context.Background(),
				&run.Run{ID: "run_apply", Tool: "bash", Command: command}, 0, true)
			if err != nil || preview.Outcome != ApplyHeld {
				t.Fatalf("PreviewApply() = %+v, %v, want a hold", preview, err)
			}
			return preview.Rule
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies, _ := settingsStore(t, test.Body, "")
			d := New(run.NewMemStore(), &fakeRunnerLister{hosts: []string{"a"}}, nil,
				WithPolicies(policies), WithNoJanitor())
			t.Cleanup(d.Close)
			got := test.Text(t, d)
			if !strings.Contains(got, "script reads") {
				t.Fatalf("the policy's text was not given at all: %q", got)
			}
			if strings.Contains(got, secret) {
				t.Errorf("the caller is shown the run's secret in plain text: %q", got)
			}
			if !strings.Contains(got, maskToken) {
				t.Errorf("the text %q does not show where the secret was masked", got)
			}
		})
	}
}

// TestAPipelineRecordsEachStepsNotesUnderItsName pins notes on the workflow door. A pipeline
// records what a policy noted about each of its steps, named by the step, the pipeline itself is not
// held for a note, and each step run takes back its own notes and none of its siblings'.
func TestAPipelineRecordsEachStepsNotesUnderItsName(t *testing.T) {
	t.Parallel()
	policies, bundle := settingsStore(t,
		`warn contains "a shell step" if input.run.tool == "bash"`, noteSettings)
	d := New(run.NewMemStore(), okRunner(), nil, WithPolicies(policies), WithNoJanitor())
	t.Cleanup(d.Close)
	steps := []run.PipelineStep{
		{Name: "lint", Tool: "bash", Command: "echo lint"},
		{Name: "report", Tool: "python", Command: "print(1)", DependsOn: []string{"lint"}},
	}
	parent, err := d.SubmitPipeline(context.Background(), "nightly", "", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status == run.StatusPendingApproval {
		t.Fatalf("a pipeline was held by %q for a note", parent.HeldByPolicy)
	}
	note := "rego gate (a shell step, " + bundle + ")"
	if diff := cmp.Diff([]string{`step "lint": ` + note}, parent.PolicyNotes); diff != "" {
		t.Errorf("the pipeline's notes mismatch (-want +got):\n%s", diff)
	}
	tests := []struct {
		// Step is the step built.
		Step int
		// WantNotes are the notes its run carries.
		WantNotes []string
	}{{ // Test 0: The noted step carries its note, without the step prefix.
		Step: 0, WantNotes: []string{note},
	}, { // Test 1: Its sibling carries nothing.
		Step: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			child := stepRun(parent, steps[test.Step], test.Step, 0, baseStepVars(parent))
			got := child.PolicyNotes
			if diff := cmp.Diff(test.WantNotes, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("step notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAShardCarriesItsParentsNotes pins that every shard of a noted split says what the split says,
// since each shard is the noted run over part of its hosts, and that a retry of its failed shards,
// which is judged again, carries what that judgment noted down to its own shards.
func TestAShardCarriesItsParentsNotes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policies, bundle := settingsStore(t, `warn contains "fans out" if input.run.tool == "ansible"`,
		noteSettings)
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}, nil,
		WithPolicies(policies))
	t.Cleanup(d.Close)
	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	want := []string{"rego gate (fans out, " + bundle + ")"}
	if diff := cmp.Diff(want, parent.PolicyNotes); diff != "" {
		t.Errorf("the split's notes mismatch (-want +got):\n%s", diff)
	}
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil || len(shards) < 2 {
		t.Fatalf("Shards() = %d shards, %v, want two", len(shards), err)
	}
	for _, s := range shards {
		if diff := cmp.Diff(want, s.PolicyNotes); diff != "" {
			t.Errorf("shard %s notes mismatch (-want +got):\n%s", s.ID, diff)
		}
	}

	// Failed by hand and rolled up by the coordinator, the same race-free way the retry
	// inheritance test produces something to retry.
	for _, s := range shards {
		s.Status = run.StatusFailed
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save(shard) error = %v", err)
		}
	}
	waitForStatus(t, store, parent.ID, run.StatusFailed)
	retry, err := d.RetryFailedShards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if diff := cmp.Diff(want, retry.PolicyNotes); diff != "" {
		t.Errorf("the retry's notes mismatch (-want +got):\n%s", diff)
	}
	retried, err := store.Shards(ctx, retry.ID)
	if err != nil || len(retried) == 0 {
		t.Fatalf("Shards(retry) = %d shards, %v, want some", len(retried), err)
	}
	for _, s := range retried {
		if diff := cmp.Diff(want, s.PolicyNotes); diff != "" {
			t.Errorf("retry shard %s notes mismatch (-want +got):\n%s", s.ID, diff)
		}
	}
}

// TestAProposedApplyCarriesItsNote pins notes at the plan gate, on both ways an apply is proposed.
// A destroy threshold written as a noted warning lets the apply run and leaves the count it noted on
// the run that destroys, where the same bundle under the default holds the apply instead.
func TestAProposedApplyCarriesItsNote(t *testing.T) {
	t.Parallel()
	body := `plan_gate if input.run.tool == "terraform"

warn contains msg if {
	input.plan.planned
	input.plan.destroys > 3
	msg := sprintf("plan destroys %d", [input.plan.destroys])
}`
	tests := []struct {
		// Settings are the policy entry's settings.
		Settings string
		// WantHeld is whether the proposed apply waits for a person.
		WantHeld bool
		// WantNote is whether the proposed apply carries the note.
		WantNote bool
	}{{ // Test 0: Noted: the apply goes ahead carrying the count.
		Settings: noteSettings, WantNote: true,
	}, { // Test 1: The default: the apply is held on the warning.
		Settings: "", WantHeld: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies, bundle := settingsStore(t, body, test.Settings)
			list, err := policies.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var wantNotes []string
			if test.WantNote {
				wantNotes = []string{"rego gate (plan destroys 5, " + bundle + ")"}
			}

			// In process: the plan gate plans, then submits the apply through the dispatcher.
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &planGateRunner{summary: "Plan: 0 to add, 0 to change, 5 to destroy.\n"}
			d := New(store, runner, nil, WithPolicies(policies))
			t.Cleanup(d.Close)
			created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
				run.WithCommand("infra/prod"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			waitTerminal(t, store, created.ID)
			proposal, err := store.Get(ctx, waitProposal(t, store, created.ID).ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			if held := proposal.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("in process: held = %v (%q), want %v", held, proposal.HeldByPolicy,
					test.WantHeld)
			}
			if diff := cmp.Diff(wantNotes, proposal.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("in process: notes mismatch (-want +got):\n%s", diff)
			}

			// Across the relay: the control node builds the apply from the plan a worker ran.
			plan := &run.Run{ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/prod",
				DryRun: true, Status: run.StatusSucceeded, CreatedAt: time.Now()}
			relayed, _, err := ProposeApplyFor(ctx, run.NewMemStore(), list, plan, 5, true,
				"sealed-plan-file")
			if err != nil {
				t.Fatalf("ProposeApplyFor() error = %v", err)
			}
			if held := relayed.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("relayed: held = %v (%q), want %v", held, relayed.HeldByPolicy,
					test.WantHeld)
			}
			if diff := cmp.Diff(wantNotes, relayed.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("relayed: notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
