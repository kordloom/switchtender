package dispatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// Event scripts captured from ansible core 2.21.1 with the callback plugin in
// internal/roundhouse/plugins enabled. They are the fixtures the zero-host decision turns on, so
// they are recorded here as the tool actually wrote them rather than paraphrased. Each script is
// newline delimited JSON, one event per line, so the long lines below are the wire format and
// cannot be wrapped.
const (
	// emptyRecapEvents is what a playbook whose host pattern matched nothing leaves behind: a play
	// started, then a recap naming no host. ansible-playbook exits zero for it.
	emptyRecapEvents = `{"type":"play_start","ts":1719000000,"play":"nothing"}
{"type":"stats","ts":1719000001,"stats":{}}
`
	// skippedEveryTaskEvents is a real run against a real host whose only task was skipped by a when
	// clause. The host is still named in the recap, with its skipped count, which is exactly what
	// separates it from a run that matched no host.
	skippedEveryTaskEvents = `{"type":"play_start","ts":1719000000,"play":"skip everything"}
{"type":"task_start","ts":1719000001,"play":"skip everything","task":"never runs"}
{"type":"runner_skipped","ts":1719000002,"play":"skip everything","task":"never runs","host":"web01"}
{"type":"stats","ts":1719000003,"stats":{"web01":{"ok":0,"changed":0,"failures":0,"unreachable":0,"skipped":1}}}
`
	// changedOneHostEvents is an ordinary successful run that changed one host.
	changedOneHostEvents = `{"type":"play_start","ts":1719000000,"play":"site"}
{"type":"task_start","ts":1719000001,"play":"site","task":"install"}
{"type":"runner_ok","ts":1719000002,"play":"site","task":"install","host":"web01","changed":true}
{"type":"stats","ts":1719000003,"stats":{"web01":{"ok":1,"changed":1,"failures":0,"unreachable":0,"skipped":0}}}
`
	// failedOneHostEvents is a genuine failure: the host is named in the recap with a failure,
	// and ansible-playbook exits nonzero for it.
	failedOneHostEvents = `{"type":"play_start","ts":1719000000,"play":"site"}
{"type":"task_start","ts":1719000001,"play":"site","task":"install"}
{"type":"runner_failed","ts":1719000002,"play":"site","task":"install","host":"web01","message":"boom"}
{"type":"stats","ts":1719000003,"stats":{"web01":{"ok":0,"changed":0,"failures":1,"unreachable":0,"skipped":0}}}
`
)

// recapRunner is a Runner that writes a fixed event script to the run's sidecar file and reports a
// fixed exit code, so a test can pin the recap a run leaves behind. It lists hosts too, so the same
// fixture drives a split.
type recapRunner struct {
	// events is the newline delimited event script written to the run's sidecar file. An empty
	// script writes no file, which is what a run with event capture unavailable leaves behind.
	events string
	// hosts is the inventory a split shards over.
	hosts []string
	// exitCode is what the tool reports when it finishes.
	exitCode int
}

// Run writes the event script, emits a line of output, and reports the fixed exit code.
func (r *recapRunner) Run(
	_ context.Context, spec roundhouse.Spec, out io.Writer,
) (roundhouse.Result, error) {
	_, _ = io.WriteString(out, "runner output\n")
	if r.events != "" && spec.EventsPath != "" {
		if err := os.WriteFile(spec.EventsPath, []byte(r.events), 0o600); err != nil {
			return roundhouse.Result{}, err
		}
	}
	return roundhouse.Result{ExitCode: r.exitCode}, nil
}

// Hosts returns the fixed host set a split shards over.
func (r *recapRunner) Hosts(context.Context, string, string) ([]string, error) {
	return r.hosts, nil
}

// TestARunThatTouchedNoHostIsNotASuccess pins the outcome this product cannot afford to get wrong.
//
// Ansible exits zero when a host pattern matches nothing: it warns, prints a recap naming no host,
// and stops. The outcome used to be read from the exit code alone, so such a run was recorded as
// succeeded, signed, and linked into the chain. Nothing ran. The proof was valid and it was a proof
// of nothing, which is the one failure a product that sells "prove every change" cannot have.
//
// It lives in this package rather than in internal/run because the defect is in the outcome
// decision, which the dispatcher owns. internal/run holds the other half, the fold that reports
// the recap.
//
// The cases that must not move are as load bearing as the one that must. A play that ran against a
// real host and skipped every task is a real run: its host is named in the recap with a skipped
// count, so it stays a success. A run that reported no recap at all is left alone as well, because
// that is what event capture being unavailable looks like, and unproven is not proven empty. Bash,
// Terraform, and the other command tools have no host pattern and no recap, so nothing here reaches
// them.
//
// A tag filter that leaves no task produces the same empty recap even though the hosts matched,
// measured against ansible core 2.21.1, so it is caught here too and is pinned as its own case.
func TestARunThatTouchedNoHostIsNotASuccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Tool       string
		Command    string
		Events     string
		WantStatus run.Status
		WantError  string
		Tags       []string
		WantExit   int
		ExitCode   int
	}{{ // Test 0: The recap named no host, so the clean exit is not believed.
		Name: "ansible matched no host", Tool: run.ToolAnsible, Events: emptyRecapEvents,
		ExitCode: 0, WantStatus: run.StatusFailed, WantError: errNoHostTouched, WantExit: 0,
	}, { // Test 1: Real hosts, every task skipped. A real run, and still a success.
		Name: "ansible skipped every task on a real host", Tool: run.ToolAnsible,
		Events: skippedEveryTaskEvents, ExitCode: 0, WantStatus: run.StatusSucceeded, WantExit: 0,
	}, { // Test 2: An ordinary successful run is untouched.
		Name: "ansible changed a host", Tool: run.ToolAnsible, Events: changedOneHostEvents,
		ExitCode: 0, WantStatus: run.StatusSucceeded, WantExit: 0,
	}, { // Test 3: A genuine failure is untouched.
		Name: "ansible failed on a host", Tool: run.ToolAnsible, Events: failedOneHostEvents,
		ExitCode: 2, WantStatus: run.StatusFailed, WantExit: 2,
	}, { // Test 4: No recap at all proves nothing either way, so nothing is concluded from it.
		Name: "ansible reported no recap", Tool: run.ToolAnsible, ExitCode: 0,
		WantStatus: run.StatusSucceeded, WantExit: 0,
	}, { // Test 5: A nonzero exit is already an honest failure, so no reason is invented for it.
		Name: "ansible matched no host and exited nonzero", Tool: run.ToolAnsible,
		Events: emptyRecapEvents, ExitCode: 2, WantStatus: run.StatusFailed, WantExit: 2,
	}, { // Test 6: A bash run has no host pattern and no recap.
		Name: "bash", Tool: run.ToolBash, Command: "echo hi", ExitCode: 0,
		WantStatus: run.StatusSucceeded, WantExit: 0,
	}, { // Test 7: Neither has Terraform.
		Name: "terraform", Tool: run.ToolTerraform, Command: "/tmp/stack", ExitCode: 0,
		WantStatus: run.StatusSucceeded, WantExit: 0,
	}, { // Test 8: The hosts matched, but a tag filter left no task, so the recap named nobody.
		Name: "ansible tags matched no task", Tool: run.ToolAnsible, Events: emptyRecapEvents,
		Tags: []string{"nosuchtag"}, ExitCode: 0, WantStatus: run.StatusFailed,
		WantError: errNoHostTouched, WantExit: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			runner := &recapRunner{events: test.Events, exitCode: test.ExitCode}
			d := New(store, runner, nil, WithNotifyClient(http.DefaultClient))
			defer d.Close()

			playbook := "site.yml"
			if test.Tool != run.ToolAnsible {
				playbook = ""
			}
			created, err := d.Submit(context.Background(), playbook, "inv",
				run.WithTool(test.Tool), run.WithCommand(test.Command), run.WithTags(test.Tags...))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := waitTerminal(t, store, created.ID)

			if diff := cmp.Diff(test.WantStatus, got.Status, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantError, got.Error, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("error mismatch (-want +got):\n%s", diff)
			}
			if got.ExitCode == nil || *got.ExitCode != test.WantExit {
				t.Errorf("exit code = %v, want %d, so the record shows what the tool reported",
					got.ExitCode, test.WantExit)
			}
		})
	}
}

// TestSplitWhoseShardsTouchedNoHostFails pins that the sharded path carries the same guarantee.
//
// A split's parent finalizes succeeded only when every shard succeeded, and each shard executes
// through the same single-run path, so a shard that touched no host fails on its own and the parent
// follows. That is why dispatch_split.go needs no zero-host check of its own, and this test is what
// keeps that true rather than merely believed.
func TestSplitWhoseShardsTouchedNoHostFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Events     string
		WantStatus run.Status
		WantShard  run.Status
		WantError  string
	}{{ // Test 0: No shard touched a host, so neither the shards nor the split are a success.
		Name: "no shard touched a host", Events: emptyRecapEvents,
		WantStatus: run.StatusFailed, WantShard: run.StatusFailed, WantError: errNoHostTouched,
	}, { // Test 1: The control. Shards that touched hosts still roll up to a successful split.
		Name: "every shard touched a host", Events: changedOneHostEvents,
		WantStatus: run.StatusSucceeded, WantShard: run.StatusSucceeded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &recapRunner{events: test.Events, hosts: []string{"h1", "h2", "h3", "h4"}}
			d := New(store, runner, nil, WithNotifyClient(http.DefaultClient))
			defer d.Close()

			parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			got := waitTerminal(t, store, parent.ID)
			if diff := cmp.Diff(test.WantStatus, got.Status, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parent status mismatch (-want +got):\n%s", diff)
			}

			shards, err := store.Shards(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Shards() error = %v", err)
			}
			if len(shards) != 2 {
				t.Fatalf("shards = %d, want 2", len(shards))
			}
			for _, s := range shards {
				if diff := cmp.Diff(test.WantShard, s.Status, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("shard %s status mismatch (-want +got):\n%s", s.ID, diff)
				}
				if diff := cmp.Diff(test.WantError, s.Error, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("shard %s error mismatch (-want +got):\n%s", s.ID, diff)
				}
			}
		})
	}
}

// TestPipelineStepThatTouchedNoHostFailsThePipeline pins the other place a coordinator finalizes a
// parent succeeded. A step is a child run, so it fails on its own and the pipeline stops at it.
func TestPipelineStepThatTouchedNoHostFailsThePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &recapRunner{events: emptyRecapEvents}, nil, WithNotifyClient(http.DefaultClient))
	defer d.Close()

	steps := []run.PipelineStep{
		{Name: "first", Playbook: "one.yml"},
		{Name: "second", Playbook: "two.yml"},
	}
	parent, err := d.SubmitPipeline(ctx, "deploy", "inv", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("pipeline status = %q, want failed, since its first step touched no host", got.Status)
	}

	children, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(children) == 0 {
		t.Fatal("the pipeline recorded no steps, so this proves nothing")
	}
	if children[0].Error != errNoHostTouched {
		t.Errorf("step error = %q, want the zero-host reason", children[0].Error)
	}
}
