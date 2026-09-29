package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// hostScriptedRunner fails any execution whose host limit names a host in bad and succeeds for every
// other, recording each limit it was asked to run so a test can read how the fleet was divided.
type hostScriptedRunner struct {
	// hosts is the inventory the split fans out over.
	hosts []string
	// bad names the hosts whose shard must fail.
	bad map[string]bool
	// mu guards limits.
	mu sync.Mutex
	// limits records the host group of every execution, in the order they arrived.
	limits []string
}

// Run fails when the limit names a bad host and succeeds otherwise.
func (h *hostScriptedRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	h.mu.Lock()
	h.limits = append(h.limits, spec.Limit)
	h.mu.Unlock()
	_, _ = io.WriteString(out, "ran "+spec.Limit)
	for _, host := range strings.Split(spec.Limit, ",") {
		if h.bad[host] {
			return roundhouse.Result{ExitCode: 2}, nil
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts reports the inventory so the dispatcher can shard it.
func (h *hostScriptedRunner) Hosts(context.Context, string, string) ([]string, error) {
	return h.hosts, nil
}

// seenLimits returns a copy of the host groups that were executed.
func (h *hostScriptedRunner) seenLimits() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.limits...)
}

// TestAFailingShardDoesNotStopItsSiblings pins failure isolation across a split. The whole point of
// fanning a change out is that one bad host group is contained: the shards that had nothing to do
// with the failure must still run, still record their own result, and still keep their own hosts. A
// coordinator that aborted the rest on the first failure would leave most of the fleet in a state
// nobody chose, and the record would not say which hosts were reached.
func TestAFailingShardDoesNotStopItsSiblings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	hosts := []string{"web01", "web02", "web03", "web04"}
	runner := &hostScriptedRunner{hosts: hosts, bad: map[string]bool{"web03": true}}
	d := New(store, runner, nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 4)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	gotParent := waitTerminal(t, store, parent.ID)
	if gotParent.Status != run.StatusFailed {
		t.Errorf("split status = %q, want failed: one shard failed", gotParent.Status)
	}

	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 4 {
		t.Fatalf("shard count = %d, want 4", len(shards))
	}

	var covered []string
	failed, succeeded := 0, 0
	for _, s := range shards {
		if !s.Status.Terminal() {
			t.Errorf("shard %s is %q, so a sibling's failure left it unresolved", s.ID, s.Status)
		}
		covered = append(covered, strings.Split(s.Limit, ",")...)
		switch s.Status {
		case run.StatusFailed:
			failed++
			if !strings.Contains(s.Limit, "web03") {
				t.Errorf("shard limited to %q failed, but its hosts were all healthy", s.Limit)
			}
		case run.StatusSucceeded:
			succeeded++
			if strings.Contains(s.Limit, "web03") {
				t.Errorf("shard limited to %q succeeded, but it holds the failing host", s.Limit)
			}
		default:
			t.Errorf("shard %s ended %q, want succeeded or failed", s.ID, s.Status)
		}
	}
	if failed != 1 {
		t.Errorf("%d shards failed, want exactly the one holding the failing host", failed)
	}
	if succeeded != 3 {
		t.Errorf("%d shards succeeded, want the three that never touched the failing host", succeeded)
	}

	// Every host is covered exactly once, so a failure neither dropped hosts nor ran one twice.
	sort.Strings(covered)
	if diff := cmp.Diff(hosts, covered, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("hosts covered by the shards (-want +got):\n%s", diff)
	}
	if got := len(runner.seenLimits()); got != 4 {
		t.Errorf("%d executions, want one per shard: a failure must not stop the others from running",
			got)
	}
}

// TestOneShardFailureLeavesTheOthersOwnEvidence is the record half of the same guarantee. A
// succeeded shard's own log and exit code must survive its sibling's failure, because "which hosts
// actually got the change" is answered from the shards, not from the parent's rollup.
func TestOneShardFailureLeavesTheOthersOwnEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &hostScriptedRunner{
		hosts: []string{"db01", "db02"},
		bad:   map[string]bool{"db02": true},
	}
	d := New(store, runner, nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	waitTerminal(t, store, parent.ID)

	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		body, lerr := store.Log(ctx, s.ID)
		if lerr != nil {
			t.Fatalf("Log(%s) error = %v", s.ID, lerr)
		}
		if !strings.Contains(string(body), s.Limit) {
			t.Errorf("shard %s limited to %q has log %q, so its own output was lost",
				s.ID, s.Limit, body)
		}
		if s.ExitCode == nil {
			t.Errorf("shard %s recorded no exit code, so nothing says how it ended", s.ID)
			continue
		}
		wantCode := 0
		if s.Status == run.StatusFailed {
			wantCode = 2
		}
		if *s.ExitCode != wantCode {
			t.Errorf("shard %s exit code = %d, want %d", s.ID, *s.ExitCode, wantCode)
		}
	}
}

// TestRetryFailedShardsRetriesAnOrphanUnderAHealthyParent covers the branch between the two rules
// already pinned elsewhere. The orphan sweep stamps its own reason on a shard it canceled before it
// started, and that reason is what separates a crash orphan from a person's cancel. A parent that
// itself finished normally can still hold one, and it has to be retried: nothing ran on those hosts.
func TestRetryFailedShardsRetriesAnOrphanUnderAHealthyParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"web01", "web02", "web03"}}, nil,
		WithNoJanitor())
	defer d.Close()

	parentID := "run_healthy_parent"
	parent := &run.Run{
		ID: parentID, Kind: run.KindSplit, Status: run.StatusFailed,
		Playbook: "site.yml", Inventory: "inv", CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	count := 3
	shards := []*run.Run{
		{Status: run.StatusSucceeded, Limit: "web01"},
		{Status: run.StatusCanceled, Error: run.OrphanError(), Limit: "web02"},
		{Status: run.StatusCanceled, Error: "", Limit: "web03"},
	}
	for i, s := range shards {
		idx := i
		s.ID = fmt.Sprintf("%s_c%d", parentID, i)
		s.ParentID = &parentID
		s.ShardIndex = &idx
		s.ShardCount = &count
		s.CreatedAt = time.Now()
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save(%s) error = %v", s.ID, err)
		}
	}

	retry, err := d.RetryFailedShards(ctx, parentID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	children, err := store.Shards(ctx, retry.ID)
	if err != nil {
		t.Fatalf("Shards(retry) error = %v", err)
	}
	var limits []string
	for _, c := range children {
		limits = append(limits, c.Limit)
	}
	sort.Strings(limits)
	if diff := cmp.Diff([]string{"web02"}, limits, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("retried host groups (-want +got):\n%s\nonly the shard the orphan sweep canceled "+
			"never ran, so it is the only one to run again", diff)
	}
}

// TestRetryFailedShardsRefusals walks every reason the retry declines. Each one protects a different
// thing: retrying a run that is not a split has no shards to reason about, retrying one still in
// flight would double up on hosts it is changing right now, and retrying a split with nothing failed
// would re-run a green change for no reason.
func TestRetryFailedShardsRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which refusal is being pinned.
		Name string
		// Parent is the run the retry targets, or nil to target a run that does not exist.
		Parent *run.Run
		// Shards are the parent's stored shards.
		Shards []*run.Run
		// Want is the error the retry must return.
		Want error
	}{{ // Test 0: A plain run has no shards, so there is nothing to retry.
		Name:   "not a split",
		Parent: &run.Run{Kind: "", Status: run.StatusFailed},
		Want:   ErrNotSplit,
	}, { // Test 1: A split still running would be retried onto hosts it is changing right now.
		Name:   "still running",
		Parent: &run.Run{Kind: run.KindSplit, Status: run.StatusRunning},
		Want:   ErrNotFinished,
	}, { // Test 2: A split still waiting for an approver has not run at all.
		Name:   "still awaiting approval",
		Parent: &run.Run{Kind: run.KindSplit, Status: run.StatusPendingApproval},
		Want:   ErrNotFinished,
	}, { // Test 3: A green split has nothing that needs running again.
		Name:   "nothing failed",
		Parent: &run.Run{Kind: run.KindSplit, Status: run.StatusSucceeded},
		Shards: []*run.Run{{Status: run.StatusSucceeded, Limit: "web01"}},
		Want:   ErrNoFailedShards,
	}, { // Test 4: A rejected split's shards were canceled by the rejection, not by a failure.
		Name:   "a rejected split",
		Parent: &run.Run{Kind: run.KindSplit, Status: run.StatusRejected},
		Shards: []*run.Run{{Status: run.StatusCanceled, Error: "canceled: rejected", Limit: "web01"}},
		Want:   ErrNoFailedShards,
	}, { // Test 5: A split with no stored shards at all has nothing to retry.
		Name:   "no shards stored",
		Parent: &run.Run{Kind: run.KindSplit, Status: run.StatusFailed},
		Want:   ErrNoFailedShards,
	}, { // Test 6: A run that does not exist is reported as missing rather than retried blindly.
		Name: "no such run", Want: run.ErrNotFound,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &fakeRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
			defer d.Close()

			parentID := fmt.Sprintf("run_parent_%d", testNum)
			if test.Parent != nil {
				test.Parent.ID = parentID
				test.Parent.Playbook = "site.yml"
				test.Parent.Inventory = "inv"
				test.Parent.CreatedAt = time.Now()
				if err := store.Save(ctx, test.Parent); err != nil {
					t.Fatalf("Save(parent) error = %v", err)
				}
			}
			count := len(test.Shards)
			for i, s := range test.Shards {
				idx := i
				s.ID = fmt.Sprintf("%s_c%d", parentID, i)
				s.ParentID = &parentID
				s.ShardIndex = &idx
				s.ShardCount = &count
				s.CreatedAt = time.Now()
				if err := store.Save(ctx, s); err != nil {
					t.Fatalf("Save(%s) error = %v", s.ID, err)
				}
			}

			got, err := d.RetryFailedShards(ctx, parentID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("RetryFailedShards() error = %v, want %v", err, test.Want)
			}
			if got != nil {
				t.Errorf("a refused retry returned run %s, want nothing created", got.ID)
			}
		})
	}
}

// saveFinishedRun stores a run with its per-host results. The summaries are written while the run is
// still non-terminal, because the store fences auxiliary writes to a finished run, which is the same
// order the executor writes them in.
func saveFinishedRun(t *testing.T, store run.Store, r *run.Run, summaries []run.HostSummary) {
	t.Helper()
	ctx := context.Background()
	final := r.Status
	r.Status = run.StatusRunning
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) error = %v", r.ID, err)
	}
	if len(summaries) > 0 {
		if err := store.SaveHostSummary(ctx, r.ID, summaries); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", r.ID, err)
		}
	}
	r.Status = final
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) terminal error = %v", r.ID, err)
	}
}

// TestRelaunchFailedHostsRefusals walks the refusals of the failed-host relaunch. The relaunch reads
// per-host results to decide what to target, so a run that recorded none has no notion of a failed
// host at all, and relaunching one anyway would run the whole fleet under the name of a repair.
func TestRelaunchFailedHostsRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which refusal is being pinned.
		Name string
		// Status is the source run's status.
		Status run.Status
		// Summaries are the per-host results the source run recorded.
		Summaries []run.HostSummary
		// Want is the error the relaunch must return.
		Want error
	}{{ // Test 0: A run still executing has not finished failing yet.
		Name: "still running", Status: run.StatusRunning, Want: ErrNotFinished,
	}, { // Test 1: A run still waiting for a decision has run nothing.
		Name: "awaiting approval", Status: run.StatusPendingApproval, Want: ErrNotFinished,
	}, { // Test 2: A finished run with no per-host results, such as one bash command.
		Name: "no per-host results", Status: run.StatusFailed, Want: ErrNoHostSummary,
	}, { // Test 3: A finished run whose hosts all came back clean has nothing to repair.
		Name: "every host clean", Status: run.StatusSucceeded,
		Summaries: []run.HostSummary{{Host: "web01", OK: 3}, {Host: "web02", OK: 3}},
		Want:      ErrNoFailedHosts,
	}, { // Test 4: Skipped and changed tasks are not failures, so they trigger no relaunch.
		Name: "changed and skipped only", Status: run.StatusSucceeded,
		Summaries: []run.HostSummary{{Host: "web01", Changed: 2, Skipped: 5}},
		Want:      ErrNoFailedHosts,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, okRunner(), nil, WithNoJanitor())
			defer d.Close()

			id := fmt.Sprintf("run_src_%d", testNum)
			saveFinishedRun(t, store, &run.Run{
				ID: id, Playbook: "site.yml", Inventory: "inv", Status: test.Status,
				CreatedAt: time.Now(),
			}, test.Summaries)

			got, err := d.RelaunchFailedHosts(ctx, id, "casey", "session")
			if !errors.Is(err, test.Want) {
				t.Fatalf("RelaunchFailedHosts() error = %v, want %v", err, test.Want)
			}
			if got != nil {
				t.Errorf("a refused relaunch created run %s", got.ID)
			}
		})
	}

	// A run that does not exist is reported as missing rather than relaunched against nothing.
	t.Run("test 5", func(t *testing.T) {
		t.Parallel()
		store := run.NewMemStore()
		d := New(store, okRunner(), nil, WithNoJanitor())
		defer d.Close()
		if _, err := d.RelaunchFailedHosts(context.Background(), "run_absent", "casey",
			"session"); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("RelaunchFailedHosts() error = %v, want %v", err, run.ErrNotFound)
		}
	})
}

// TestRelaunchFailedHostsTargetsOnlyTheHostsThatBroke pins the host selection and the attribution.
// Targeting more than the broken hosts would re-run a change on hosts that already took it, and
// stamping the original run's actor would credit the repair to whoever ran the thing that failed
// rather than to whoever asked for the repair.
func TestRelaunchFailedHostsTargetsOnlyTheHostsThatBroke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	src := &run.Run{
		ID: "run_source", Playbook: "site.yml", Inventory: "inv", Status: run.StatusFailed,
		CreatedAt: time.Now(), Actor: "original-operator", ActorType: "session", OrgID: "org_a",
	}
	saveFinishedRun(t, store, src, []run.HostSummary{
		{Host: "web01", OK: 4},
		{Host: "web02", Failures: 1},
		{Host: "web03", Unreachable: 1},
		{Host: "web04", OK: 2, Skipped: 1},
	})

	got, err := d.RelaunchFailedHosts(ctx, src.ID, "repair-operator", "token")
	if err != nil {
		t.Fatalf("RelaunchFailedHosts() error = %v", err)
	}

	limits := strings.Split(got.Limit, ",")
	sort.Strings(limits)
	if diff := cmp.Diff([]string{"web02", "web03"}, limits, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("relaunch host limit (-want +got):\n%s\na relaunch must touch only the hosts that "+
			"failed or were unreachable", diff)
	}
	if got.RetryOf == nil || *got.RetryOf != src.ID {
		t.Errorf("RetryOf = %v, want the run this repairs (%s)", got.RetryOf, src.ID)
	}
	if got.Actor != "repair-operator" {
		t.Errorf("Actor = %q, want the person who asked for the relaunch, not the one who ran the "+
			"original", got.Actor)
	}
	if got.ActorType != "token" {
		t.Errorf("ActorType = %q, want token", got.ActorType)
	}
	if got.OrgID != "org_a" {
		t.Errorf("OrgID = %q, want the source run's tenant: a relaunch of an objectless run would "+
			"otherwise be readable across every tenant", got.OrgID)
	}
}

// TestRelaunchFailedHostsDedupesADoubleClick pins the dedupe window on the relaunch. The button is in
// the interface beside a failed run, so a double click is the expected input, and two relaunches would
// run the same repair twice on the same broken hosts.
func TestRelaunchFailedHostsDedupesADoubleClick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	src := &run.Run{
		ID: "run_double", Playbook: "site.yml", Inventory: "inv", Status: run.StatusFailed,
		CreatedAt: time.Now(),
	}
	saveFinishedRun(t, store, src, []run.HostSummary{{Host: "web02", Failures: 1}})

	first, err := d.RelaunchFailedHosts(ctx, src.ID, "casey", "session")
	if err != nil {
		t.Fatalf("RelaunchFailedHosts(first) error = %v", err)
	}
	second, err := d.RelaunchFailedHosts(ctx, src.ID, "casey", "session")
	if err != nil {
		t.Fatalf("RelaunchFailedHosts(second) error = %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("a second relaunch created run %s, want the first one (%s) returned so a double "+
			"click cannot fire two repairs at the same hosts", second.ID, first.ID)
	}
}

// TestRetryFailedShardsDedupesConcurrentClicks proves the dedupe holds when the two clicks race
// rather than arriving in sequence. The pre-check cannot settle a race on its own, so the store's
// unique index is the backstop, and both callers must resolve to one retry.
func TestRetryFailedShardsDedupesConcurrentClicks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor(),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	parentID := "run_race_parent"
	parent := &run.Run{
		ID: parentID, Kind: run.KindSplit, Status: run.StatusFailed, Playbook: "site.yml",
		Inventory: "inv", CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	idx, count := 0, 1
	shard := &run.Run{
		ID: parentID + "_c0", Status: run.StatusFailed, Limit: "web01", ParentID: &parentID,
		ShardIndex: &idx, ShardCount: &count, CreatedAt: time.Now(), Playbook: "site.yml",
	}
	if err := store.Save(ctx, shard); err != nil {
		t.Fatalf("Save(shard) error = %v", err)
	}

	const clicks = 6
	ids := make([]string, clicks)
	errs := make([]error, clicks)
	var wg sync.WaitGroup
	wg.Add(clicks)
	for i := range clicks {
		go func(i int) {
			defer wg.Done()
			retry, err := d.RetryFailedShards(ctx, parentID)
			errs[i] = err
			if retry != nil {
				ids[i] = retry.ID
			}
		}(i)
	}
	wg.Wait()

	first := ""
	for i, err := range errs {
		if err != nil {
			t.Fatalf("click %d error = %v", i, err)
		}
		if first == "" {
			first = ids[i]
		}
		if ids[i] != first {
			t.Errorf("click %d produced retry %s, want the single retry %s: two retries mean the "+
				"same host group runs twice", i, ids[i], first)
		}
	}
}
