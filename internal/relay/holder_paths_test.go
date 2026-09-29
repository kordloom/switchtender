package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// leasedPost issues an authenticated request that also presents a per-claim capability, returning the
// status and the body, which the endpoints that mint or terminalize both need read.
func leasedPost(t *testing.T, base, path, lease, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", bearer)
	if lease != "" {
		req.Header.Set(leaseHeader, lease)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return resp.StatusCode, string(out)
}

// TestAHeartbeatCannotRenewAnotherExecutorsLease pins that renewing a lease answers the same holder
// question every other call does.
//
// A heartbeat is what keeps a run out of the janitor's reclaim sweep, so renewing somebody else's is
// how one worker holds a run alive that it is not executing, indefinitely, while the executor that
// actually holds it may already be gone. The refusal is answered as not-found rather than as a
// mismatch, so a caller learns nothing about a run it does not hold.
func TestAHeartbeatCannotRenewAnotherExecutorsLease(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_held")

	tests := []struct {
		Name       string
		Owner      string
		WantStatus int
	}{{ // Test 0: The holder renews its own lease.
		Name: "holder", Owner: "worker-a", WantStatus: http.StatusNoContent,
	}, { // Test 1: Another executor sharing the same worker token cannot.
		Name: "another executor", Owner: "worker-b", WantStatus: http.StatusNotFound,
	}, { // Test 2: Nor can a caller that asserts no name at all.
		Name: "no owner", Owner: "", WantStatus: http.StatusNotFound,
	}, { // Test 3: The name is normalized on both sides, so a near miss is still a miss.
		Name: "near miss", Owner: "worker-a2", WantStatus: http.StatusNotFound,
	}, { // Test 4: Nor can a caller naming a different run entirely.
		Name: "different run", Owner: "worker-a", WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			id := "run_held"
			if test.Name == "different run" {
				id = "run_never_existed"
			}
			body := fmt.Sprintf(`{"id":%q,"owner":%q}`, id, test.Owner)
			status, msg := post(t, fixture.URL, http.MethodPost, "/relay/v1/heartbeat", bearer, body)
			if status != test.WantStatus {
				t.Errorf("a heartbeat from %s answered %d (%s), want %d", test.Name, status, msg,
					test.WantStatus)
			}
		})
	}
}

// TestAStoreFaultIsNotAnsweredAsAMissingRun pins that a control node which cannot read is answered as
// a fault, while a run that genuinely is not there is answered as not found. A worker treats not-found
// as "this run is gone, stop working it", so a store outage read that way would make every worker in
// the estate abandon the runs it is executing.
func TestAStoreFaultIsNotAnsweredAsAMissingRun(t *testing.T) {
	t.Parallel()
	store := newFaultyStore(t)
	fixture := newRelayFixture(t, store, nil)
	seedClaimed(t, fixture.Store, "run_unreadable")

	store.failGet = true
	status, body := post(t, fixture.URL, http.MethodGet, "/relay/v1/runs/run_unreadable", bearer, "")
	if status == http.StatusNotFound {
		t.Fatal("a store fault while reading a run answered 404, so a worker abandons a run it is " +
			"executing because the control node had a bad minute")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("reading a run under a store fault answered %d (%s), want 500", status, body)
	}
	if strings.Contains(body, errFaulty.Error()) {
		t.Errorf("the response put the store's own error text on the wire: %s", body)
	}

	// A heartbeat whose renewal itself faults is a fault too, not a lost lease.
	store.failGet = false
	store.failHeartbeat = true
	status, body = post(t, fixture.URL, http.MethodPost, "/relay/v1/heartbeat", bearer,
		`{"id":"run_unreadable","owner":"worker-a"}`)
	if status != http.StatusInternalServerError {
		t.Errorf("a heartbeat under a store fault answered %d (%s), want 500", status, body)
	}
}

// TestALogWriteFaultAndAPurgedRunAreToldApart pins the two ways a record write can fail after the
// holder checks have passed. A purged run is a 404, which tells the worker the run is gone; anything
// else is a 500, which tells it to retry. Collapsing them would either strand output the store could
// have taken or make a worker retry a run that no longer exists for the whole abandon window.
func TestALogWriteFaultAndAPurgedRunAreToldApart(t *testing.T) {
	t.Parallel()
	store := newFaultyStore(t)
	fixture := newRelayFixture(t, store, nil)
	seedClaimed(t, fixture.Store, "run_writes")

	store.appendLogErr = run.ErrNotFound
	status, body := post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_writes/log", bearer,
		"output\n")
	if status != http.StatusNotFound {
		t.Errorf("a log write for a purged run answered %d (%s), want 404", status, body)
	}

	store.appendLogErr = errFaulty
	status, body = post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_writes/log", bearer,
		"output\n")
	if status != http.StatusInternalServerError {
		t.Errorf("a log write under a store fault answered %d (%s), want 500", status, body)
	}
	if strings.Contains(body, errFaulty.Error()) {
		t.Errorf("the response put the store's own error text on the wire: %s", body)
	}

	store.appendLogErr = nil
	store.appendEventsErr = run.ErrNotFound
	status, body = post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_writes/events", bearer,
		`[{"type":"runner_ok","host":"web01"}]`)
	if status != http.StatusNotFound {
		t.Errorf("an event write for a purged run answered %d (%s), want 404", status, body)
	}
}

// TestAFastWorkerCanReportTerminalBeforeAnyProgress pins the double swap the terminal save performs.
//
// A claimed run stays pending until its first progress report, and a run that finishes quickly may
// never send one. Without stepping the run to running first, the fenced finalize finds nothing to move
// and the report is refused, so the fastest runs in the estate are the ones whose outcomes are lost.
// Both moves are compare-and-swaps, so a run a janitor sweep settled still fails them both.
func TestAFastWorkerCanReportTerminalBeforeAnyProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRelayFixture(t, nil, nil)
	if err := fixture.Store.Save(ctx, &run.Run{ID: "run_fast", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	claimed, err := fixture.Store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.Status != run.StatusPending {
		t.Fatalf("a claimed run is %q, so this test no longer covers the pending-to-terminal path",
			claimed.Status)
	}

	status, body := leasedPost(t, fixture.URL, "/relay/v1/runs/run_fast/save",
		claimed.ClaimSecret, `{"status":"succeeded","claimed_by":"worker-a"}`)
	if status != http.StatusNoContent {
		t.Fatalf("a terminal report before any progress answered %d (%s), want 204: the fastest "+
			"runs in the estate would be the ones whose outcomes are lost", status, body)
	}
	got, err := fixture.Store.Get(ctx, "run_fast")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(run.StatusSucceeded, got.Status); diff != "" {
		t.Errorf("stored status mismatch (-want +got):\n%s", diff)
	}
}

// TestAChildRunsOutcomeIsLeftToItsCoordinator pins that a shard or pipeline step does not commit its
// own outcome entry when it finishes across the relay. The parent's coordinator rolls its children's
// results into the one entry the parent commits, so committing here as well would put two entries in
// the chain for one piece of work and make the parent's receipt disagree with its children's.
func TestAChildRunsOutcomeIsLeftToItsCoordinator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	parent := "run_parent"
	if err := backing.Save(ctx, &run.Run{ID: "run_child", Playbook: "site.yml",
		Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a",
		ParentID: &parent}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	ts := relayWithAudits(t, backing, audits)

	status, body := post(t, ts, http.MethodPost, "/relay/v1/runs/run_child/save", bearer,
		`{"status":"succeeded","claimed_by":"worker-a"}`)
	if status != http.StatusNoContent {
		t.Fatalf("a child run's terminal report answered %d (%s), want 204", status, body)
	}
	entries, err := audits.List(ctx, 100)
	if err != nil {
		t.Fatalf("audit List() error = %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Path, "/outcome") || strings.Contains(e.Path, "outcome/") {
			t.Errorf("a child run committed its own outcome entry at %q, so the chain holds two "+
				"records for one piece of work", e.Path)
		}
	}
	// The boundary crossing is still recorded, because the run did finish on a machine the control
	// node cannot reach and that is the thing worth writing down.
	var sawFinish bool
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "/relay/finished/run_child/") {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Error("no entry records the child run finishing across the relay")
	}
}

// relayWithAudits stands up a relay server over the given store and audit store and returns its base
// URL.
func relayWithAudits(t *testing.T, store run.Store, audits audit.Store) string {
	t.Helper()
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil, nil,
		audits))
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestListPoliciesAnswersAnArrayEvenWhenTheStoreHasNone pins the JSON shape of an empty policy list. A
// store that answers nil would otherwise be written out as null, which decodes on the worker as a nil
// slice rather than an empty one, and a caller that distinguishes the two would read "no rules" as
// "the answer was missing".
func TestListPoliciesAnswersAnArrayEvenWhenTheStoreHasNone(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nilPolicies{})
	status, body := post(t, fixture.URL, http.MethodGet, "/relay/v1/policies", bearer, "")
	if status != http.StatusOK {
		t.Fatalf("policies answered %d (%s), want 200", status, body)
	}
	if diff := cmp.Diff("[]", strings.TrimSpace(body)); diff != "" {
		t.Errorf("empty policy list mismatch (-want +got):\n%s", diff)
	}
	// And a worker reading it gets an empty list rather than an error.
	tr := relay.NewHTTPTransport(fixture.URL, testWorkerToken, nil)
	got, err := tr.Policies(context.Background())
	if err != nil {
		t.Fatalf("Policies() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("policies = %d, want none", len(got))
	}
}

// nilPolicies is a policy.Store whose list succeeds and answers nil, which is what a store with no
// rows returns before anything normalizes it.
type nilPolicies struct{}

// List answers no policies and no error.
func (nilPolicies) List(context.Context) ([]*policy.Policy, error) { return nil, nil }

// Get answers not found.
func (nilPolicies) Get(context.Context, string) (*policy.Policy, error) {
	return nil, policy.ErrNotFound
}

// Save refuses.
func (nilPolicies) Save(context.Context, *policy.Policy) error { return policy.ErrReadOnly }

// Delete refuses.
func (nilPolicies) Delete(context.Context, string) error { return policy.ErrReadOnly }

// seedPlan stores a live terraform plan a worker holds, with the per-claim capability set, and returns
// that capability.
func seedPlan(t *testing.T, store run.Store, id string, shape func(*run.Run)) string {
	t.Helper()
	r := &run.Run{ID: id, Tool: run.ToolTerraform, Command: "infra/prod", DryRun: true,
		Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a",
		ClaimSecret: "capability-for-" + id}
	if shape != nil {
		shape(r)
	}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	return r.ClaimSecret
}

// TestProposeApplyRefusesARunThatIsNotAPlanInHand pins the remaining shapes a proposal is refused
// from. The apply is a clone of the named run with the dry-run flag forced off, so the run has to be
// the thing that clone is meant to be. A run held for a decision is not planning anything, and
// building an apply from it would put a second real change behind the decision a person was asked to
// make; a run that is itself a proposed apply would propose an endless chain of them.
func TestProposeApplyRefusesARunThatIsNotAPlanInHand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Shape func(*run.Run)
		Name  string
	}{{ // Test 0: A run waiting on a person is not a plan proposing anything.
		Name:  "held for approval",
		Shape: func(r *run.Run) { r.Status = run.StatusPendingApproval },
	}, { // Test 1: A run that is itself a proposed apply does not propose another.
		Name:  "already a proposal",
		Shape: func(r *run.Run) { r.ProposedFrom = "run_earlier_plan" },
	}, { // Test 2: A run of a tool that has no apply at all.
		Name:  "not an infrastructure tool",
		Shape: func(r *run.Run) { r.Tool = run.ToolBash; r.Command = "echo hi" },
	}, { // Test 3: A run with no tool named, which means Ansible, not terraform.
		Name:  "default tool",
		Shape: func(r *run.Run) { r.Tool = ""; r.Playbook = "site.yml" },
	}, { // Test 4: A real execution rather than a plan.
		Name:  "not a plan",
		Shape: func(r *run.Run) { r.DryRun = false },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fixture := newRelayFixture(t, nil, nil)
			lease := seedPlan(t, fixture.Store, "run_plan", test.Shape)
			status, body := leasedPost(t, fixture.URL, "/relay/v1/runs/run_plan/propose-apply",
				lease, `{"destroys":0,"read":true}`)
			if status == http.StatusCreated {
				t.Fatalf("an apply was created from %s, so a worker minted a real change from a "+
					"run nobody gated: %s", test.Name, body)
			}
			if status != http.StatusConflict {
				t.Errorf("%s answered %d (%s), want 409", test.Name, status, body)
			}
		})
	}
}

// TestAProposedApplyIsHeldWhenTheGateCouldNotBeEvaluated pins the fail-closed rule on the one endpoint
// that causes a run to exist.
//
// The apply is built from the plan with the dry-run flag forced off, so it is a real change. The rules
// that decide whether it waits for a person are read here, on the control node, rather than taken from
// the worker. A control node that cannot read them has not evaluated the gate, and a gate that could
// not be evaluated has not been passed, so the apply is held rather than queued.
func TestAProposedApplyIsHeldWhenTheGateCouldNotBeEvaluated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Policies policy.Store
		Name     string
		Body     string
	}{{ // Test 0: The control node's own policy source is down.
		Name: "policy store unreachable", Policies: unreachablePolicies{},
		Body: `{"destroys":0,"read":true}`,
	}, { // Test 1: The worker could not read the plan's summary, so nothing was weighed.
		Name: "plan summary unreadable", Policies: policy.NewMemStore(),
		Body: `{"destroys":0,"read":false}`,
	}, { // Test 2: No policy store is configured at all, so nothing could weigh the plan either.
		Name: "no policy store", Policies: nil, Body: `{"destroys":0,"read":true}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fixture := newRelayFixture(t, nil, test.Policies)
			lease := seedPlan(t, fixture.Store, "run_plan", nil)
			status, body := leasedPost(t, fixture.URL, "/relay/v1/runs/run_plan/propose-apply",
				lease, test.Body)
			if status != http.StatusCreated {
				t.Fatalf("propose apply answered %d (%s), want 201", status, body)
			}
			var proposal run.Run
			if err := json.Unmarshal([]byte(body), &proposal); err != nil {
				t.Fatalf("decode proposal error = %v from %s", err, body)
			}
			if proposal.Status != run.StatusPendingApproval {
				t.Errorf("the apply proposed under %s is %q, want pending_approval: a real change "+
					"was queued behind a gate nobody could evaluate", test.Name, proposal.Status)
			}
			if proposal.DryRun {
				t.Error("the proposed apply is still a plan, so nothing would ever change")
			}
			if diff := cmp.Diff("run_plan", proposal.ProposedFrom); diff != "" {
				t.Errorf("the apply does not name the plan it came from (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAMalformedBaseURLFailsEveryCallRatherThanPanicking pins what a worker configured with a base URL
// that cannot be made into a request does. The constructor only checks that the URL is not empty, so a
// value carrying a control character or a bad scheme reaches request building, and the failure has to
// be an error on the call rather than a panic inside the dispatcher's loop.
func TestAMalformedBaseURLFailsEveryCallRatherThanPanicking(t *testing.T) {
	t.Parallel()
	tr := relay.NewHTTPTransport("http://relay.invalid/\x7f\x01", testWorkerToken, nil)
	for name, err := range callEachTransportMethod(context.Background(), tr,
		runningRun("run_malformed")) {
		if err == nil {
			t.Errorf("%s answered nil against a base URL that cannot be made into a request", name)
			continue
		}
		if !strings.Contains(err.Error(), "build request") &&
			!strings.Contains(err.Error(), "do request") {
			t.Errorf("%s error = %q, want it to name the request it could not build or issue",
				name, err)
		}
	}
}

// TestATerminalSaveUnderACanceledContextReturnsPromptly pins that a worker shutting down is not held
// open by the window a terminal save waits for the run's last output.
//
// That window exists so a brief fault does not close a run's record over a log missing its end. It is
// ten seconds per run, and a worker draining several runs while its context is canceled would sit
// there for all of them. Cancellation has to end the wait, and it has to end it with the cancellation
// rather than with a claim that the tail was delivered.
func TestATerminalSaveUnderACanceledContextReturnsPromptly(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, http.StatusInternalServerError, `{"error":"down"}`)
	if err := tr.AppendLog(context.Background(), "run_drain", []byte("tail\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	finished := runningRun("run_drain")
	finished.Status = run.StatusSucceeded
	start := time.Now()
	err := tr.Save(ctx, finished)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Save() answered nil under a canceled context against a failing relay")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Save() error = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Save() took %v under a canceled context, so a draining worker waits out the "+
			"whole tail window for every run it holds", elapsed)
	}
}

// TestAClaimAndItsHeartbeatAgreeOnTheNormalizedName pins that the lease name is normalized at both
// endpoints that accept one. A worker asserts whatever name its configuration gave it, the claim
// stores the normalized form, and the heartbeat matches on it. Normalizing in only one place would
// make every worker whose name carries a character outside the allowed alphabet unable to renew the
// lease it just took, so its runs would be reclaimed out from under it while it was still executing.
func TestAClaimAndItsHeartbeatAgreeOnTheNormalizedName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRelayFixture(t, nil, nil)
	if err := fixture.Store.Save(ctx, &run.Run{ID: "run_named", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	tr := relay.NewHTTPTransport(fixture.URL, testWorkerToken, nil)
	// A name carrying characters the chain cannot take, which the claim rewrites on the way in.
	const asserted = "worker a:1 host.local"
	if _, err := tr.Claim(ctx, asserted, []string{""}); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	stored, err := fixture.Store.Get(ctx, "run_named")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff("worker_a_1_host.local", stored.ClaimedBy); diff != "" {
		t.Errorf("stored lease name mismatch (-want +got):\n%s", diff)
	}
	// The worker renews with the same name it asserted, and the two have to agree.
	if err := tr.Heartbeat(ctx, "run_named", asserted); err != nil {
		t.Fatalf("Heartbeat() asserting the same name as the claim error = %v: the worker cannot "+
			"renew the lease it just took, so its run is reclaimed while it is still executing", err)
	}
	// The name the worker asserted is not what identifies it: the capability minted at the claim is.
	// A worker that presents the right name without that capability is refused.
	status, body := post(t, fixture.URL, http.MethodPost, "/relay/v1/heartbeat", bearer,
		fmt.Sprintf(`{"id":"run_named","owner":%q}`, asserted))
	if status != http.StatusNotFound {
		t.Errorf("a heartbeat carrying the right name but no capability answered %d (%s), want "+
			"404: the lease name is asserted, not proven", status, body)
	}
}

// refusingAudits is an audit store whose append always fails, standing in for a trail that is full,
// locked, or on a disk that has gone away.
type refusingAudits struct {
	// Store serves the reads; only the append is broken.
	audit.Store
}

// Append refuses.
func (refusingAudits) Append(context.Context, *audit.Entry) error { return errFaulty }

// TestAnUnhealthyAuditStoreDoesNotLoseAWorkersOutcome pins the one place in the product where the
// audit append is deliberately not fail closed, and pins that it stays that way.
//
// The API refuses a mutation it cannot record, because refusing prevents the change. A worker's report
// is different: the run already happened, on hosts the control node cannot reach, so refusing the
// report does not un-finish it. It loses the outcome of work that is already done and leaves the run
// looking abandoned. The failure is logged loudly instead, and the report still lands.
func TestAnUnhealthyAuditStoreDoesNotLoseAWorkersOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	if err := backing.Save(ctx, &run.Run{ID: "run_untrailed", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	base := relayWithAudits(t, backing, refusingAudits{Store: audit.NewMemStore()})

	// The claim is recorded, and the record failing must not refuse the claim.
	status, body := post(t, base, http.MethodPost, "/relay/v1/claim", bearer,
		`{"owner":"worker-a","queues":[""]}`)
	if status != http.StatusOK {
		t.Fatalf("a claim answered %d (%s) with the audit store refusing, so work stops moving "+
			"because the trail is unhealthy", status, body)
	}
	claimed, err := backing.Get(ctx, "run_untrailed")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	// The terminal report commits an outcome as well as a record, and both failing must not refuse it.
	status, body = leasedPost(t, base, "/relay/v1/runs/run_untrailed/save", claimed.ClaimSecret,
		`{"status":"succeeded","claimed_by":"worker-a"}`)
	if status != http.StatusNoContent {
		t.Fatalf("a terminal report answered %d (%s) with the audit store refusing, so the "+
			"outcome of work that already happened is thrown away", status, body)
	}
	got, err := backing.Get(ctx, "run_untrailed")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(run.StatusSucceeded, got.Status); diff != "" {
		t.Errorf("stored status mismatch (-want +got):\n%s", diff)
	}
}

// TestProposeApplyOnARunTheControlNodeDoesNotHold pins that the endpoint which creates runs refuses
// one for a plan that is not there. It is the only path by which a worker causes a run to exist, so an
// unknown id must answer not-found rather than building an apply out of nothing.
func TestProposeApplyOnARunTheControlNodeDoesNotHold(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	status, body := leasedPost(t, fixture.URL, "/relay/v1/runs/run_absent/propose-apply",
		"any-capability", `{"destroys":0,"read":true}`)
	if status != http.StatusNotFound {
		t.Errorf("propose apply for an unknown plan answered %d (%s), want 404", status, body)
	}
	// And the store gained nothing from the attempt.
	all, err := fixture.Store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 0 {
		t.Errorf("the store holds %d runs after a proposal from a plan that does not exist", len(all))
	}
}
