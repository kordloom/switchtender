package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestSubmitRefusesARunItCannotExecute pins the input gate every submission passes through. It runs
// before anything is stored, so a run that names a tool this build does not have, or omits the one
// thing its tool needs, is refused rather than stored as pending and then claimed by a worker that
// cannot run it, where it would fail on a host with a message about the executor rather than about
// the request.
func TestSubmitRefusesARunItCannotExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the submission got wrong.
		Name string
		// Playbook is the first Submit argument.
		Playbook string
		// Opts configure the run.
		Opts []run.SubmitOption
		// Want is the error the submit must return.
		Want error
	}{{ // Test 0: A tool nothing in this build knows how to run.
		Name: "an unknown tool", Opts: []run.SubmitOption{run.WithTool("kubectl-apply")},
		Want: ErrUnknownTool,
	}, { // Test 1: A tool name that is only close to a real one.
		Name: "a misspelled tool", Opts: []run.SubmitOption{run.WithTool("terrafrom"),
			run.WithCommand("infra/prod")},
		Want: ErrUnknownTool,
	}, { // Test 2: Case matters, so a differently cased tool is not silently accepted.
		Name: "a differently cased tool", Opts: []run.SubmitOption{run.WithTool("Bash"),
			run.WithCommand("echo hi")},
		Want: ErrUnknownTool,
	}, { // Test 3: Ansible with no playbook has nothing to run.
		Name: "ansible with no playbook", Opts: []run.SubmitOption{run.WithTool(run.ToolAnsible)},
		Want: ErrNoPlaybook,
	}, { // Test 4: An empty tool defaults to Ansible, so it needs a playbook too.
		Name: "an empty tool with no playbook", Want: ErrNoPlaybook,
	}, { // Test 5: Bash with no command has no script.
		Name: "bash with no command", Opts: []run.SubmitOption{run.WithTool(run.ToolBash)},
		Want: ErrNoCommand,
	}, { // Test 6: Terraform with no working directory.
		Name: "terraform with no command", Opts: []run.SubmitOption{run.WithTool(run.ToolTerraform)},
		Want: ErrNoCommand,
	}, { // Test 7: OpenTofu likewise.
		Name: "opentofu with no command", Opts: []run.SubmitOption{run.WithTool(run.ToolOpenTofu)},
		Want: ErrNoCommand,
	}, { // Test 8: Python likewise.
		Name: "python with no command", Opts: []run.SubmitOption{run.WithTool(run.ToolPython)},
		Want: ErrNoCommand,
	}, { // Test 9: PowerShell likewise.
		Name: "powershell with no command",
		Opts: []run.SubmitOption{run.WithTool(run.ToolPowerShell)}, Want: ErrNoCommand,
	}, { // Test 10: Go likewise.
		Name: "go with no command", Opts: []run.SubmitOption{run.WithTool(run.ToolGo)},
		Want: ErrNoCommand,
	}, { // Test 11: A playbook does not stand in for a bash script.
		Name: "bash given a playbook instead of a command", Playbook: "site.yml",
		Opts: []run.SubmitOption{run.WithTool(run.ToolBash)}, Want: ErrNoCommand,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			d := New(store, okRunner(), nil, WithNoJanitor())
			defer d.Close()

			got, err := d.Submit(context.Background(), test.Playbook, "inv", test.Opts...)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Submit() error = %v, want %v", err, test.Want)
			}
			if got != nil {
				t.Errorf("a refused submission returned run %s", got.ID)
			}
			runs, lerr := store.List(context.Background())
			if lerr != nil {
				t.Fatalf("List() error = %v", lerr)
			}
			if len(runs) != 0 {
				t.Errorf("%d runs stored from a refused submission, so a worker can claim work the "+
					"request was never allowed to create", len(runs))
			}
		})
	}
}

// TestSubmitSplitRefusesWhatSubmitRefuses pins that sharding is not a way around the input gate.
// SubmitSplit is a separate entry point, and a check that lives only in Submit is a check an operator
// skips by asking for two shards.
func TestSubmitSplitRefusesWhatSubmitRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the submission got wrong.
		Name string
		// Playbook is the first SubmitSplit argument.
		Playbook string
		// Shards is how many groups were asked for.
		Shards int
		// Opts configure the run.
		Opts []run.SubmitOption
		// Want is the error the submit must return.
		Want error
	}{{ // Test 0: An Ansible split with no playbook, which is what a split runs.
		Name: "no playbook", Shards: 4, Want: ErrNoPlaybook,
	}, { // Test 1: Below two shards it falls back to a single run, which still needs a playbook.
		Name: "one shard and no playbook", Shards: 1, Want: ErrNoPlaybook,
	}, { // Test 2: A non-Ansible tool falls back to a single run, which still needs its command.
		Name: "bash with no command", Shards: 4,
		Opts: []run.SubmitOption{run.WithTool(run.ToolBash)}, Want: ErrNoCommand,
	}, { // Test 3: An unknown tool is refused on this path too.
		Name: "an unknown tool", Shards: 4,
		Opts: []run.SubmitOption{run.WithTool("kubectl-apply")}, Want: ErrUnknownTool,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			d := New(store, &fakeRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
			defer d.Close()

			got, err := d.SubmitSplit(context.Background(), test.Playbook, "inv", test.Shards,
				test.Opts...)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SubmitSplit() error = %v, want %v", err, test.Want)
			}
			if got != nil {
				t.Errorf("a refused split returned run %s", got.ID)
			}
		})
	}
}

// TestSubmitSplitWithoutAHostListerIsRefused pins the one dependency sharding needs. A runner that
// cannot enumerate the inventory cannot divide it, and guessing host groups would run the change
// against a set nobody chose.
func TestSubmitSplitWithoutAHostListerIsRefused(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	// okRunner is a plain runner with no Hosts method, which is exactly the case.
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	got, err := d.SubmitSplit(context.Background(), "site.yml", "inv", 4)
	if !errors.Is(err, ErrNoHostLister) {
		t.Fatalf("SubmitSplit() error = %v, want %v", err, ErrNoHostLister)
	}
	if got != nil {
		t.Errorf("a refused split returned run %s", got.ID)
	}
}

// TestSubmitSplitFallsBackToOneRunForASmallInventory pins the lower bound on sharding. Dividing one
// host into several groups produces empty shards, each of which would run the playbook against
// nothing while the parent waited on all of them.
func TestSubmitSplitFallsBackToOneRunForASmallInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which lower bound is in play.
		Name string
		// Hosts is the inventory the runner reports.
		Hosts []string
		// Shards is how many groups were asked for.
		Shards int
	}{
		{Name: "no hosts", Hosts: nil, Shards: 4},                        // Test 0: An empty inventory.
		{Name: "one host", Hosts: []string{"web01"}, Shards: 4},          // Test 1: A single host.
		{Name: "one shard", Hosts: []string{"a", "b"}, Shards: 1},        // Test 2: One group is not a split.
		{Name: "zero shards", Hosts: []string{"a", "b"}, Shards: 0},      // Test 3: Nor is zero.
		{Name: "negative shards", Hosts: []string{"a", "b"}, Shards: -3}, // Test 4: Nor a negative.
	}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			d := New(store, &fakeRunnerLister{hosts: test.Hosts}, nil, WithNoJanitor(),
				WithClaimInterval(time.Millisecond))
			defer d.Close()

			got, err := d.SubmitSplit(context.Background(), "site.yml", "inv", test.Shards)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			if got.Kind == run.KindSplit {
				t.Errorf("kind = %q, want a plain run: an inventory of %d host(s) split into %d "+
					"groups produces shards targeting nothing", got.Kind, len(test.Hosts),
					test.Shards)
			}
			if got.ShardCount != nil {
				t.Errorf("ShardCount = %d, want none on a plain run", *got.ShardCount)
			}
		})
	}
}

// TestPartitionBoundaries pins the host packer at the sizes a real fleet hits. It decides which hosts
// a shard changes, so a group that loses a host silently skips it and a duplicated host takes the
// change twice from two workers at once.
func TestPartitionBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which shape is being packed.
		Name string
		// Hosts is the inventory.
		Hosts []string
		// Shards is the requested group count.
		Shards int
		// Costs is the recorded per-host duration history.
		Costs map[string]float64
		// WantGroups is how many groups should come back.
		WantGroups int
	}{{ // Test 0: No hosts produces no groups rather than empty ones.
		Name: "no hosts", Hosts: nil, Shards: 4, WantGroups: 0,
	}, { // Test 1: One host is one group.
		Name: "one host", Hosts: []string{"a"}, Shards: 4, WantGroups: 1,
	}, { // Test 2: More shards than hosts is clamped to the host count.
		Name: "more shards than hosts", Hosts: []string{"a", "b", "c"}, Shards: 10, WantGroups: 3,
	}, { // Test 3: An even split with no cost history balances by count.
		Name: "even split, no history", Hosts: []string{"a", "b", "c", "d"}, Shards: 2,
		WantGroups: 2,
	}, { // Test 4: Zero and negative costs are ignored, so they cannot invert the packing.
		Name: "zero and negative costs", Hosts: []string{"a", "b", "c", "d"}, Shards: 2,
		Costs:      map[string]float64{"a": 0, "b": -5, "c": 100, "d": 1},
		WantGroups: 2,
	}, { // Test 5: A cost for a host that is not in the inventory is harmless.
		Name: "costs for absent hosts", Hosts: []string{"a", "b"}, Shards: 2,
		Costs:      map[string]float64{"zzz": 900},
		WantGroups: 2,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			groups := partition(test.Hosts, test.Shards, test.Costs)
			if len(groups) != test.WantGroups {
				t.Fatalf("groups = %d, want %d", len(groups), test.WantGroups)
			}

			seen := make(map[string]int, len(test.Hosts))
			for i, g := range groups {
				if len(g) == 0 {
					t.Errorf("group %d is empty, so a shard would run the playbook against nothing", i)
				}
				for _, h := range g {
					seen[h]++
				}
			}
			for _, h := range test.Hosts {
				switch seen[h] {
				case 1:
				case 0:
					t.Errorf("host %q is in no shard, so it silently skips the change", h)
				default:
					t.Errorf("host %q is in %d shards, so two workers change it at once", h, seen[h])
				}
			}
			if len(seen) != len(test.Hosts) {
				t.Errorf("the groups name %d distinct hosts, want %d", len(seen), len(test.Hosts))
			}
		})
	}
}

// TestPartitionIsDeterministic pins that the same inventory and the same cost history always produce
// the same groups. A shard's host group is recorded on its run and is what a retry re-runs, so a
// packer whose output depended on map iteration order would retry a different set of hosts than the
// one that failed.
func TestPartitionIsDeterministic(t *testing.T) {
	t.Parallel()
	hosts := []string{"web01", "web02", "web03", "web04", "db01", "db02", "cache01"}
	costs := map[string]float64{"web01": 12, "db01": 40, "cache01": 3, "web03": 12}

	first := partition(hosts, 3, costs)
	for i := range 25 {
		got := partition(hosts, 3, costs)
		if len(got) != len(first) {
			t.Fatalf("run %d produced %d groups, want %d", i, len(got), len(first))
		}
		for g := range first {
			if fmt.Sprint(got[g]) != fmt.Sprint(first[g]) {
				t.Fatalf("run %d group %d = %v, want %v: a shard's host group is what a retry "+
					"re-runs, so it cannot vary between calls", i, g, got[g], first[g])
			}
		}
	}
}

// TestConcurrentSubmitsAllReachTheStore drives the submit path the way a burst from a scheduler does.
// Every submission has to produce its own run: a lost one is work an operator asked for that nothing
// records and nothing runs. It exists to be run under the race detector.
func TestConcurrentSubmitsAllReachTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithWorkers(4),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	const submissions = 40
	ids := make([]string, submissions)
	errs := make([]error, submissions)
	var wg sync.WaitGroup
	wg.Add(submissions)
	for i := range submissions {
		go func(i int) {
			defer wg.Done()
			created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand(fmt.Sprintf("echo %d", i)))
			errs[i] = err
			if created != nil {
				ids[i] = created.ID
			}
		}(i)
	}
	wg.Wait()

	unique := make(map[string]struct{}, submissions)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submission %d error = %v", i, err)
		}
		if ids[i] == "" {
			t.Fatalf("submission %d produced no run", i)
		}
		if _, dup := unique[ids[i]]; dup {
			t.Errorf("submission %d reused run id %s, so two submissions share one record",
				i, ids[i])
		}
		unique[ids[i]] = struct{}{}
	}
	if len(unique) != submissions {
		t.Errorf("%d distinct runs from %d submissions", len(unique), submissions)
	}
	for _, id := range ids {
		waitTerminal(t, store, id)
	}
}

// TestSplitRollupKeepsAFinishedShardsOwnResult demonstrates a defect in the coordinator's rollup.
//
// The poll skips recording any child status on the tick where it first notices the parent was
// canceled, then immediately reports every status it has not recorded as stopped. A shard that had
// already succeeded and was sitting terminal in the store is therefore rolled up as canceled, and the
// parent's outcome, which is what the audit chain commits, states that a change which fully landed on
// every host did not happen. The documented rule is narrower than that: only a child not yet terminal
// should be reported canceled.
//
// The window is one poll interval wide, which is exactly the gap between a fan-out finishing and
// somebody clicking cancel on a run they think is still going.
func TestSplitRollupKeepsAFinishedShardsOwnResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_rollup_truth"
	ended := time.Now()
	count := 2
	ids := make([]string, count)
	for i := range count {
		idx := i
		ids[i] = fmt.Sprintf("%s_c%d", parentID, i)
		if err := store.Save(ctx, &run.Run{
			ID: ids[i], Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: ended,
			EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", ids[i], err)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	got := d.waitChildren(canceled, ids)

	for i, status := range got {
		if status != run.StatusSucceeded {
			t.Errorf("shard %d rolled up as %q while the store holds it succeeded: the parent's "+
				"outcome would state that a change which landed on every host did not happen",
				i, status)
		}
	}
}
