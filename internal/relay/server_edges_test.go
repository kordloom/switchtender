package relay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// errFaulty is the fault a faultyStore raises for whichever read a test made fail.
var errFaulty = errors.New("the store is unavailable")

// faultyStore is a working store with individual reads made to fail, so a test can prove the relay
// answers a store fault the way it says it does rather than papering over it.
type faultyStore struct {
	// Store serves everything not deliberately broken.
	run.Store
	// SummaryAppender serves the continuation writes, which no test here breaks.
	run.SummaryAppender
	// failClaim makes Claim raise a fault that is not the empty-queue sentinel.
	failClaim bool
	// failHostSummaries makes the read that decides whether a run recorded a host raise a fault.
	failHostSummaries bool
	// failFinalize makes the fenced terminal write raise a fault.
	failFinalize bool
	// failProgress makes the fenced running write raise a fault.
	failProgress bool
	// failSaveHostSummary makes the whole-report host write raise a fault.
	failSaveHostSummary bool
	// failAppendHostSummary makes the continuation host write raise a fault.
	failAppendHostSummary bool
	// failSaveTaskSummary makes the whole-report task write raise a fault.
	failSaveTaskSummary bool
	// failAppendTaskSummary makes the continuation task write raise a fault.
	failAppendTaskSummary bool
	// failAppendEvents makes the event write raise a fault.
	failAppendEvents bool
	// failSaveHostFacts makes the facts write raise a fault.
	failSaveHostFacts bool
	// failGet makes reading a run raise a fault that is not the not-found sentinel.
	failGet bool
	// failHeartbeat makes renewing a lease raise a fault that is not the not-found sentinel.
	failHeartbeat bool
	// appendLogErr, when set, is what the log write returns instead of writing.
	appendLogErr error
	// appendEventsErr, when set, is what the event write returns instead of writing.
	appendEventsErr error
}

// Get raises a fault when the test asked for one.
func (f *faultyStore) Get(ctx context.Context, id string) (*run.Run, error) {
	if f.failGet {
		return nil, errFaulty
	}
	return f.Store.Get(ctx, id)
}

// Heartbeat raises a fault when the test asked for one.
func (f *faultyStore) Heartbeat(ctx context.Context, id, owner string) error {
	if f.failHeartbeat {
		return errFaulty
	}
	return f.Store.Heartbeat(ctx, id, owner)
}

// AppendLog returns the error the test asked for, if any.
func (f *faultyStore) AppendLog(ctx context.Context, id string, p []byte) error {
	if f.appendLogErr != nil {
		return f.appendLogErr
	}
	return f.Store.AppendLog(ctx, id, p)
}

// FinalizeRunning raises a fault when the test asked for one.
func (f *faultyStore) FinalizeRunning(ctx context.Context, id string,
	fin run.Finalization) (bool, error) {
	if f.failFinalize {
		return false, errFaulty
	}
	return f.Store.FinalizeRunning(ctx, id, fin)
}

// ApplyRunningProgress raises a fault when the test asked for one.
func (f *faultyStore) ApplyRunningProgress(ctx context.Context, id, owner string,
	p run.Progress) (bool, error) {
	if f.failProgress {
		return false, errFaulty
	}
	return f.Store.ApplyRunningProgress(ctx, id, owner, p)
}

// SaveHostSummary raises a fault when the test asked for one.
func (f *faultyStore) SaveHostSummary(ctx context.Context, runID string,
	s []run.HostSummary) error {
	if f.failSaveHostSummary {
		return errFaulty
	}
	return f.Store.SaveHostSummary(ctx, runID, s)
}

// AppendHostSummary raises a fault when the test asked for one.
func (f *faultyStore) AppendHostSummary(ctx context.Context, runID string,
	s []run.HostSummary) error {
	if f.failAppendHostSummary {
		return errFaulty
	}
	return f.SummaryAppender.AppendHostSummary(ctx, runID, s)
}

// SaveTaskSummary raises a fault when the test asked for one.
func (f *faultyStore) SaveTaskSummary(ctx context.Context, runID string,
	s []run.TaskSummary) error {
	if f.failSaveTaskSummary {
		return errFaulty
	}
	return f.Store.SaveTaskSummary(ctx, runID, s)
}

// AppendTaskSummary raises a fault when the test asked for one.
func (f *faultyStore) AppendTaskSummary(ctx context.Context, runID string,
	s []run.TaskSummary) error {
	if f.failAppendTaskSummary {
		return errFaulty
	}
	return f.SummaryAppender.AppendTaskSummary(ctx, runID, s)
}

// AppendEvents raises the fault or the sentinel the test asked for.
func (f *faultyStore) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	if f.failAppendEvents {
		return errFaulty
	}
	if f.appendEventsErr != nil {
		return f.appendEventsErr
	}
	return f.Store.AppendEvents(ctx, id, events)
}

// SaveHostFacts raises a fault when the test asked for one.
func (f *faultyStore) SaveHostFacts(ctx context.Context, runID string, facts []run.HostFacts) error {
	if f.failSaveHostFacts {
		return errFaulty
	}
	return f.Store.SaveHostFacts(ctx, runID, facts)
}

// newFaultyStore returns a faultyStore over a fresh in-memory store.
func newFaultyStore(t *testing.T) *faultyStore {
	t.Helper()
	m := run.NewMemStore()
	appender, ok := m.(run.SummaryAppender)
	if !ok {
		t.Fatal("the in-memory store is not a run.SummaryAppender")
	}
	return &faultyStore{Store: m, SummaryAppender: appender}
}

// Claim raises a fault when the test asked for one, and otherwise leases as usual.
func (f *faultyStore) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	if f.failClaim {
		return nil, errFaulty
	}
	return f.Store.Claim(ctx, owner, queues)
}

// RunHostSummaries raises a fault when the test asked for one, and otherwise reads as usual.
func (f *faultyStore) RunHostSummaries(ctx context.Context, runID string) ([]run.HostSummary, error) {
	if f.failHostSummaries {
		return nil, errFaulty
	}
	return f.Store.RunHostSummaries(ctx, runID)
}

// unreachablePolicies is a policy.Store whose list always fails, standing in for a control node whose
// own policy source is down.
type unreachablePolicies struct{}

// List raises the fault.
func (unreachablePolicies) List(context.Context) ([]*policy.Policy, error) { return nil, errFaulty }

// Get raises the fault.
func (unreachablePolicies) Get(context.Context, string) (*policy.Policy, error) {
	return nil, errFaulty
}

// Save raises the fault.
func (unreachablePolicies) Save(context.Context, *policy.Policy) error { return errFaulty }

// Delete raises the fault.
func (unreachablePolicies) Delete(context.Context, string) error { return errFaulty }

// relayFixture is a running relay server and the store behind it.
type relayFixture struct {
	// URL is the server's base URL.
	URL string
	// Store is what the handler reads and writes.
	Store run.Store
}

// newRelayFixture stands up a relay server over the given store, policy store, and pools, seeds one
// claimed running run, and returns the fixture.
func newRelayFixture(t *testing.T, store run.Store, policies policy.Store) relayFixture {
	t.Helper()
	if store == nil {
		store = run.NewMemStore()
	}
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil,
		policies, nil))
	t.Cleanup(ts.Close)
	return relayFixture{URL: ts.URL, Store: store}
}

// seedClaimed stores a run in the shape a worker reports on: claimed, running, no kind, and with no
// per-claim secret, so the older holder check applies and no capability header is needed.
func seedClaimed(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	r := &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "worker-a"}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	return r
}

// post issues an authenticated request to the relay and returns the status and body.
func post(t *testing.T, base, method, path, authorization, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, base+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
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

// bearer is the header a legitimate worker presents.
const bearer = "Bearer " + testWorkerToken

// TestRelayRefusesABodyItCannotDecode pins that every endpoint answers a malformed body with a 400
// rather than acting on the zero value it would otherwise decode.
//
// The zero value matters here in a way it usually does not. An empty claim body is a claim by an
// unnamed owner; an empty save body is a report with no status at all; an empty events array is a
// report that the run produced nothing. Each of those is a write against a real run, so a decoder
// that shrugged at a truncated body would let a dropped connection become a record of work that did
// not happen.
//
//nolint:funlen // Test function.
func TestRelayRefusesABodyItCannotDecode(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_bad")

	bodies := map[string]string{
		"empty":          "",
		"truncated":      `{"owner":"worker-a`,
		"not json":       "<html>hello</html>",
		"bare string":    `"worker-a"`,
		"trailing comma": `{"owner":"worker-a",}`,
	}
	paths := []struct {
		Name string
		Path string
	}{{
		Name: "claim", Path: "/relay/v1/claim",
	}, {
		Name: "heartbeat", Path: "/relay/v1/heartbeat",
	}, {
		Name: "save", Path: "/relay/v1/runs/run_bad/save",
	}, {
		Name: "propose apply", Path: "/relay/v1/runs/run_bad/propose-apply",
	}, {
		Name: "events", Path: "/relay/v1/runs/run_bad/events",
	}, {
		Name: "host summary", Path: "/relay/v1/runs/run_bad/host-summary",
	}, {
		Name: "host facts", Path: "/relay/v1/runs/run_bad/host-facts",
	}, {
		Name: "task summary", Path: "/relay/v1/runs/run_bad/task-summary",
	}}
	testNum := 0
	for _, p := range paths {
		for name, body := range bodies {
			num, target, doc := testNum, p, body
			t.Run(fmt.Sprintf("test %d %s %s", num, p.Name, name), func(t *testing.T) {
				t.Parallel()
				status, _ := post(t, fixture.URL, http.MethodPost, target.Path, bearer, doc)
				if status != http.StatusBadRequest {
					t.Errorf("%s with a %s body answered %d, want 400 so a dropped connection "+
						"cannot become a write against a real run", target.Name, name, status)
				}
			})
			testNum++
		}
	}
}

// TestSaveRefusesAReportAddressedToAnotherRun pins that a report has to be about the run it was sent
// to. The path names the run whose lease and queue were checked, so a body naming a different one is
// either a confused worker or an attempt to have those checks performed against one run and applied
// to another.
func TestSaveRefusesAReportAddressedToAnotherRun(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_here")
	seedClaimed(t, fixture.Store, "run_elsewhere")

	tests := []struct {
		Name       string
		Body       string
		WantStatus int
	}{{ // Test 0: A body naming another run is refused outright.
		Name: "different id", WantStatus: http.StatusBadRequest,
		Body: `{"id":"run_elsewhere","status":"running","claimed_by":"worker-a"}`,
	}, { // Test 1: A body naming this run is fine.
		Name: "matching id", WantStatus: http.StatusNoContent,
		Body: `{"id":"run_here","status":"running","claimed_by":"worker-a"}`,
	}, { // Test 2: A body naming no run at all is taken as being about the path's run.
		Name: "no id", WantStatus: http.StatusNoContent,
		Body: `{"status":"running","claimed_by":"worker-a"}`,
	}, { // Test 3: An id differing only in case is a different run, not the same one.
		Name: "case differs", WantStatus: http.StatusBadRequest,
		Body: `{"id":"RUN_HERE","status":"running","claimed_by":"worker-a"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			status, body := post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_here/save",
				bearer, test.Body)
			if status != test.WantStatus {
				t.Errorf("save with %s answered %d (%s), want %d", test.Name, status, body,
					test.WantStatus)
			}
		})
	}
	// The run named in the body was not touched by the refused report.
	got, err := fixture.Store.Get(context.Background(), "run_elsewhere")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("run_elsewhere status = %q, want it untouched by a report addressed elsewhere",
			got.Status)
	}
}

// TestEveryReportEndpointRefusesAnOversizeArray pins the element cap on all four report endpoints, not
// only the events one. A count cap is not a work cap on its own: a megabyte of empty objects becomes
// hundreds of thousands of marshals and single-row inserts inside one transaction, which on a single
// writer holds the whole control node. Any endpoint left uncapped is the one a worker token would use.
func TestEveryReportEndpointRefusesAnOversizeArray(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_flood")
	over := "[" + strings.TrimSuffix(strings.Repeat("{},",
		relay.MaxRelayElementsForTest()+1), ",") + "]"

	for testNum, path := range []string{"events", "host-summary", "host-facts", "task-summary"} {
		t.Run(fmt.Sprintf("test %d %s", testNum, path), func(t *testing.T) {
			t.Parallel()
			status, _ := post(t, fixture.URL, http.MethodPost,
				"/relay/v1/runs/run_flood/"+path, bearer, over)
			if status != http.StatusRequestEntityTooLarge {
				t.Errorf("%s with %d elements answered %d, want 413 so a worker token cannot "+
					"force an unbounded decode on the control node", path,
					relay.MaxRelayElementsForTest()+1, status)
			}
		})
	}
}

// TestAuthorizationHeaderShapes pins exactly which presented headers resolve a pool and which do not.
// The relay is the one door into the shared store from a segment the control node cannot reach, so a
// header shape accepted by accident is an open door, and one refused by accident is a dead mesh.
func TestAuthorizationHeaderShapes(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	tests := []struct {
		Name          string
		Authorization string
		WantAuthed    bool
	}{{ // Test 0: The ordinary header a worker sends.
		Name: "bearer", Authorization: "Bearer " + testWorkerToken, WantAuthed: true,
	}, { // Test 1: The scheme is matched without regard to case, as the standard requires.
		Name: "lowercase scheme", Authorization: "bearer " + testWorkerToken, WantAuthed: true,
	}, { // Test 2: Surrounding whitespace on the token is trimmed rather than refused.
		Name: "padded token", Authorization: "Bearer   " + testWorkerToken + "  ", WantAuthed: true,
	}, { // Test 3: No header at all.
		Name: "absent", Authorization: "", WantAuthed: false,
	}, { // Test 4: The scheme with nothing after it.
		Name: "scheme only", Authorization: "Bearer ", WantAuthed: false,
	}, { // Test 5: The bare word with no space.
		Name: "no separator", Authorization: "Bearer" + testWorkerToken, WantAuthed: false,
	}, { // Test 6: A different scheme carrying the right token.
		Name: "wrong scheme", Authorization: "Basic " + testWorkerToken, WantAuthed: false,
	}, { // Test 7: The raw token with no scheme.
		Name: "no scheme", Authorization: testWorkerToken, WantAuthed: false,
	}, { // Test 8: The digest of the token is not the token.
		Name: "digest not token", Authorization: "Bearer " + relay.HashToken(testWorkerToken),
		WantAuthed: false,
	}, { // Test 9: A token with the right prefix is not the right token.
		Name: "prefix", Authorization: "Bearer " + testWorkerToken[:len(testWorkerToken)-1],
		WantAuthed: false,
	}, { // Test 10: Nor is one with something appended.
		Name: "suffix", Authorization: "Bearer " + testWorkerToken + "x", WantAuthed: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			status, _ := post(t, fixture.URL, http.MethodPost, "/relay/v1/claim",
				test.Authorization, `{"owner":"worker-a","queues":[""]}`)
			authed := status != http.StatusUnauthorized
			if authed != test.WantAuthed {
				t.Errorf("%s answered %d, which reads as authed=%v, want %v", test.Name, status,
					authed, test.WantAuthed)
			}
		})
	}
}

// TestListPoliciesTellsNoRulesApartFromCannotTell pins the two answers a worker's policy read can get
// and keeps them distinct. An install with no policy store honestly has no rules, so it answers with
// an empty list. A control node whose policy source is down cannot say that, so it fails: the worker
// wraps the failure as unreachable and holds the run, because a gate that could not be evaluated has
// not been passed. Collapsing the two would apply an ungated terraform run straight to production.
func TestListPoliciesTellsNoRulesApartFromCannotTell(t *testing.T) {
	t.Parallel()
	none := newRelayFixture(t, nil, nil)
	status, body := post(t, none.URL, http.MethodGet, "/relay/v1/policies", bearer, "")
	if status != http.StatusOK {
		t.Fatalf("policies with no store answered %d (%s), want 200", status, body)
	}
	if diff := cmp.Diff("[]", strings.TrimSpace(body)); diff != "" {
		t.Errorf("an install with no policy store must answer an empty list (-want +got):\n%s", diff)
	}

	down := newRelayFixture(t, nil, unreachablePolicies{})
	status, body = post(t, down.URL, http.MethodGet, "/relay/v1/policies", bearer, "")
	if status != http.StatusInternalServerError {
		t.Fatalf("policies with a failing store answered %d (%s), want 500 so the worker holds "+
			"the run rather than reading it as an install with no rules", status, body)
	}
	if strings.Contains(body, errFaulty.Error()) {
		t.Errorf("the response carried the store's own error text %q, which puts internals on "+
			"the wire", body)
	}
}

// TestFactsAreRefusedWhenTheStoreCannotConfirmTheHost pins that the host scope check fails closed. A
// worker may write facts only for hosts its own run recorded a result for, and the check is a read
// against the store. A store that cannot answer is not a store that said yes: reading its fault as a
// pass would let one leased run replace the recorded facts for any machine in the fleet.
func TestFactsAreRefusedWhenTheStoreCannotConfirmTheHost(t *testing.T) {
	t.Parallel()
	store := newFaultyStore(t)
	fixture := newRelayFixture(t, store, nil)
	seedClaimed(t, fixture.Store, "run_facts")
	body := `[{"host":"web01","facts":{"os":"linux"}}]`

	// With the read working and no result recorded, the facts are refused and the reason is stated.
	status, msg := post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_facts/host-facts",
		bearer, body)
	if status != http.StatusForbidden {
		t.Fatalf("facts for an unrecorded host answered %d (%s), want 403", status, msg)
	}

	// With the read failing, the answer must still be a refusal rather than a pass.
	store.failHostSummaries = true
	status, msg = post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_facts/host-facts",
		bearer, body)
	if status == http.StatusNoContent {
		t.Fatal("facts were accepted while the store could not confirm the run had touched the " +
			"host, so a leased run can rewrite the fleet's recorded facts")
	}
	if status != http.StatusForbidden {
		t.Errorf("facts under a failing summary read answered %d (%s), want 403", status, msg)
	}
	// A body naming no host at all carries nothing to scope and is not refused by this check.
	store.failHostSummaries = false
	status, msg = post(t, fixture.URL, http.MethodPost, "/relay/v1/runs/run_facts/host-facts",
		bearer, `[{"host":"","facts":{"os":"linux"}}]`)
	if status != http.StatusNoContent {
		t.Errorf("facts naming no host answered %d (%s), want 204 since the store drops them "+
			"anyway", status, msg)
	}
}

// TestClaimFaultIsNotReadAsAnEmptyQueue pins that a store fault during a claim answers 500, which the
// transport surfaces as an error, rather than the 204 that means nothing is pending. A worker that
// read a broken control node as an idle queue would poll a dead install forever and report nothing.
func TestClaimFaultIsNotReadAsAnEmptyQueue(t *testing.T) {
	t.Parallel()
	store := newFaultyStore(t)
	store.failClaim = true
	fixture := newRelayFixture(t, store, nil)

	status, body := post(t, fixture.URL, http.MethodPost, "/relay/v1/claim", bearer,
		`{"owner":"worker-a","queues":[""]}`)
	if status == http.StatusNoContent {
		t.Fatal("a store fault during a claim answered 204, so a worker reads a broken control " +
			"node as an idle queue")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("claim under a store fault answered %d (%s), want 500", status, body)
	}
	if strings.Contains(body, errFaulty.Error()) {
		t.Errorf("the response carried the store's own error text %q", body)
	}
}

// TestOnlyTheDocumentedRelayRoutesAreServed pins that the relay serves the eleven routes it declares
// and nothing else. The handler is mounted outside the API's own gate, so a route reachable here by
// accident is a route reachable with a worker token, which the least trusted machine in the estate
// holds.
func TestOnlyTheDocumentedRelayRoutesAreServed(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_routes")
	tests := []struct {
		Name       string
		Method     string
		Path       string
		WantStatus int
	}{{ // Test 0: A path the relay does not declare.
		Name: "unknown path", Method: http.MethodGet, Path: "/relay/v1/runs", WantStatus: 404,
	}, { // Test 1: A neighboring version.
		Name: "other version", Method: http.MethodGet, Path: "/relay/v2/policies", WantStatus: 404,
	}, { // Test 2: The API's own routes are not reachable through the relay handler.
		Name: "api route", Method: http.MethodGet, Path: "/v1/runs", WantStatus: 404,
	}, { // Test 3: A claim is a POST, and a GET of it is not served.
		Name: "wrong method on claim", Method: http.MethodGet, Path: "/relay/v1/claim",
		WantStatus: 405,
	}, { // Test 4: A run read is a GET, and a POST of it is not served.
		Name: "wrong method on get", Method: http.MethodPost, Path: "/relay/v1/runs/run_routes",
		WantStatus: 405,
	}, { // Test 5: A DELETE of a run is not a route at all.
		Name: "delete a run", Method: http.MethodDelete, Path: "/relay/v1/runs/run_routes",
		WantStatus: 405,
	}, { // Test 6: A run read with no id names no run.
		Name: "no id", Method: http.MethodGet, Path: "/relay/v1/runs/", WantStatus: 404,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			status, body := post(t, fixture.URL, test.Method, test.Path, bearer, "")
			if status != test.WantStatus {
				t.Errorf("%s %s answered %d (%s), want %d", test.Method, test.Path, status, body,
					test.WantStatus)
			}
		})
	}
}

// TestUnauthorizedIsAnsweredBeforeAnythingIsRead pins that a request presenting no usable token is
// refused on every route, not only the claim. The gate is one middleware in front of the whole mux,
// and a route that slipped past it would be reachable by anyone who can reach the port.
func TestUnauthorizedIsAnsweredBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_guarded")
	routes := []struct {
		Method string
		Path   string
	}{
		{http.MethodGet, "/relay/v1/policies"},
		{http.MethodPost, "/relay/v1/claim"},
		{http.MethodPost, "/relay/v1/heartbeat"},
		{http.MethodGet, "/relay/v1/runs/run_guarded"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/save"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/log"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/events"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/propose-apply"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/host-summary"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/host-facts"},
		{http.MethodPost, "/relay/v1/runs/run_guarded/task-summary"},
	}
	for testNum, route := range routes {
		t.Run(fmt.Sprintf("test %d %s", testNum, route.Path), func(t *testing.T) {
			t.Parallel()
			status, body := post(t, fixture.URL, route.Method, route.Path, "", "[]")
			if status != http.StatusUnauthorized {
				t.Errorf("%s %s answered %d (%s) with no token, want 401", route.Method,
					route.Path, status, body)
			}
		})
	}
	// The run was not touched by any of those attempts.
	got, err := fixture.Store.Get(context.Background(), "run_guarded")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("run status = %q after unauthenticated writes, want it untouched", got.Status)
	}
}

// leaseRecorder records the capability header presented on every request reaching the relay, so a test
// can prove the transport attaches it to each report rather than only to one.
type leaseRecorder struct {
	// inner is the real relay handler.
	inner http.Handler
	// mu guards seen.
	mu sync.Mutex
	// seen maps a request path to the capability header it carried.
	seen map[string]string
}

// ServeHTTP records the presented capability and forwards.
func (l *leaseRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	if l.seen == nil {
		l.seen = map[string]string{}
	}
	l.seen[r.URL.Path] = r.Header.Get(leaseHeader)
	l.mu.Unlock()
	l.inner.ServeHTTP(w, r)
}

// presented returns the capability header recorded for a path suffix.
func (l *leaseRecorder) presented(suffix string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for path, lease := range l.seen {
		if strings.HasSuffix(path, suffix) {
			return lease, true
		}
	}
	return "", false
}

// TestTheClaimCapabilityRidesOnEveryReportButNeverInABody pins both halves of how the per-claim
// capability travels.
//
// Every worker presents the same shared token, so the lease name a report carries is asserted rather
// than proven, and the capability minted at claim is what makes a report provably the holder's. It has
// to reach every write a run makes, because a report path that omitted it would be the one a forged
// report used. It also has to stay out of every body: the field is json:"-" precisely so the secret is
// not in a payload a worker can log, cache, or forward, and the claim response is the only place it
// crosses the wire at all.
func TestTheClaimCapabilityRidesOnEveryReportButNeverInABody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	if err := backing.Save(ctx, &run.Run{ID: "run_cap", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	recorder := &leaseRecorder{inner: relay.NewHandler(backing, relay.SinglePool(testWorkerToken),
		nil, nil, nil)}
	ts := httptest.NewServer(recorder)
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	// The secret is not in the run the worker decoded, so it cannot be read back out of a body.
	if claimed.ClaimSecret != "" {
		t.Error("the claim response body carried the per-claim secret, so a worker can log, " +
			"cache, or forward the capability that authorizes its reports")
	}
	stored, err := backing.Get(ctx, "run_cap")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.ClaimSecret == "" {
		t.Fatal("no capability was minted for the claim")
	}

	if err := c.Heartbeat(ctx, "run_cap", "worker-a"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, "run_cap", []run.HostSummary{{Host: "web01"}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := c.SaveHostFacts(ctx, "run_cap", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"os": "linux"}}}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	if err := c.SaveTaskSummary(ctx, "run_cap", []run.TaskSummary{{Task: "ping"}}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	if err := c.AppendLog(ctx, "run_cap", []byte("out\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	claimed.Status = run.StatusSucceeded
	if err := c.Save(ctx, claimed); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	for _, suffix := range []string{"/heartbeat", "/host-summary", "/host-facts", "/task-summary",
		"/log", "/save"} {
		got, seen := recorder.presented(suffix)
		if !seen {
			t.Errorf("no request reached %s, so the report was never made", suffix)
			continue
		}
		if got != stored.ClaimSecret {
			t.Errorf("%s presented capability %q, want the one minted at claim: a report path "+
				"that omits it is the one a forged report would use", suffix, got)
		}
	}
}
