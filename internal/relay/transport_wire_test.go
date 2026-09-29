package relay_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// scriptedRelay is a stand-in control node that answers every relay path with one status and one
// body, so a test can drive the transport's response handling without a real store behind it.
type scriptedRelay struct {
	// mu guards paths.
	mu sync.Mutex
	// paths records every request path the transport issued, in order.
	paths []string
	// body is written after the status on every response.
	body string
	// status is the HTTP status every response carries.
	status int
}

// handler returns the http.Handler that answers with the scripted status and body.
func (s *scriptedRelay) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.RequestURI())
		s.mu.Unlock()
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	})
}

// seen returns how many requests reached the relay whose path contains want.
func (s *scriptedRelay) seen(want string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if strings.Contains(p, want) {
			n++
		}
	}
	return n
}

// scriptedTransport stands up a scriptedRelay and returns a transport dialing it.
func scriptedTransport(t *testing.T, status int, body string) (relay.Transport, *scriptedRelay) {
	t.Helper()
	s := &scriptedRelay{status: status, body: body}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()), s
}

// callEachTransportMethod invokes every execution-path call on tr and returns the error each one
// produced, keyed by the name the transport uses for it in its own error text.
func callEachTransportMethod(ctx context.Context, tr relay.Transport, r *run.Run) map[string]error {
	return map[string]error{
		"claim":     firstErr(tr.Claim(ctx, "worker-a", []string{""})),
		"heartbeat": tr.Heartbeat(ctx, r.ID, "worker-a"),
		"get":       firstErr(tr.Get(ctx, r.ID)),
		"policies":  policiesErr(tr, ctx),
		"save":      tr.Save(ctx, r),
		"events": tr.AppendEvents(ctx, r.ID,
			[]event.Event{{Type: event.TypeRunnerOK, Host: "web01"}}),
		"propose apply":     firstErr(tr.ProposeApply(ctx, r.ID, 1, true)),
		"save host summary": tr.SaveHostSummary(ctx, r.ID, []run.HostSummary{{Host: "web01"}}),
		"save host facts": tr.SaveHostFacts(ctx, r.ID,
			[]run.HostFacts{{Host: "web01", Facts: map[string]string{"os": "linux"}}}),
		"save task summary": tr.SaveTaskSummary(ctx, r.ID, []run.TaskSummary{{Task: "ping"}}),
	}
}

// firstErr drops a call's value and keeps its error, so the call table above stays readable.
func firstErr[T any](_ T, err error) error { return err }

// policiesErr reads the policies and keeps only the error, for the same reason.
func policiesErr(tr relay.Transport, ctx context.Context) error { //nolint:revive // Test helper.
	_, err := tr.Policies(ctx)
	return err
}

// runningRun is the shape a worker reports on: claimed, executing, not yet finished. Every call that
// takes a run in these tests uses it, so no test accidentally exercises the terminal tail window.
func runningRun(id string) *run.Run {
	return &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: time.Now(), ClaimedBy: "worker-a"}
}

// TestEveryTransportCallReportsAnUnreachableRelay pins that a severed segment surfaces as an error on
// every execution-path call rather than as a nil error, a panic, or a hang.
//
// This is the failure the relay exists to survive: the worker is in a segment the control node cannot
// reach, so the link going away is ordinary. A call that answered nil would tell the dispatcher the
// work was recorded when nothing was, and a run's outcome would be lost with no trace of the loss.
func TestEveryTransportCallReportsAnUnreachableRelay(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), relay.SinglePool(testWorkerToken),
		nil, nil, nil))
	// Nothing is listening from here on, which is what a severed segment looks like.
	ts.Close()
	tr := relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client())

	got := callEachTransportMethod(context.Background(), tr, runningRun("run_gone"))
	for name, err := range got {
		if err == nil {
			t.Errorf("%s answered nil with the control node unreachable, so a worker believes a "+
				"report landed that never left the machine", name)
		}
	}
	// An append is the one call that cannot fail, by design: the bytes are buffered and a caller's
	// retry of an append whose bytes are already held would duplicate them in the stored log.
	if err := tr.AppendLog(context.Background(), "run_gone", []byte("hello\n")); err != nil {
		t.Errorf("AppendLog() error = %v with the relay down, want nil so a retrying caller does "+
			"not duplicate output that was never lost", err)
	}
}

// TestTransportCallsRespectACanceledContext pins that a canceled context stops each call rather than
// letting it run to a network timeout. A worker shutting down or a run being canceled has to stop
// talking to the control node promptly, and a call that ignored cancellation would hold the shutdown
// open for the length of a connect timeout per call.
func TestTransportCallsRespectACanceledContext(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, http.StatusNoContent, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := callEachTransportMethod(ctx, tr, runningRun("run_ctx"))
	for name, err := range got {
		if err == nil {
			t.Errorf("%s answered nil under a canceled context", name)
			continue
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s error = %v, want it to wrap context.Canceled", name, err)
		}
	}
}

// TestUnexpectedStatusNamesTheOperationAndItsCause pins that a failed call says which call failed,
// what status it got, and what the relay said about it. Without the operation name every relay
// failure in a worker's log reads the same, and without the folded message the reason the control
// node gave is dropped on the floor exactly when an operator needs it.
func TestUnexpectedStatusNamesTheOperationAndItsCause(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, http.StatusInternalServerError, `{"error":"the store is down"}`)

	for name, err := range callEachTransportMethod(context.Background(), tr, runningRun("run_500")) {
		if err == nil {
			t.Errorf("%s answered nil on a 500", name)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, name) {
			t.Errorf("%s error = %q, want it to name the operation", name, msg)
		}
		if !strings.Contains(msg, "500") {
			t.Errorf("%s error = %q, want it to carry the status", name, msg)
		}
		if !strings.Contains(msg, "the store is down") {
			t.Errorf("%s error = %q, want it to fold in what the relay said", name, msg)
		}
	}
}

// TestUnexpectedStatusSurvivesABodyItCannotRead pins that a relay answering with something other than
// the JSON error envelope still produces a usable error. A proxy or load balancer in front of the
// control node answers with HTML, and a transport that tried to fold that in would either fail while
// building the error or put a page of markup into a worker's log.
func TestUnexpectedStatusSurvivesABodyItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
	}{{ // Test 0: An HTML error page from a proxy in front of the control node.
		Name: "html", Body: "<html><body>502 Bad Gateway</body></html>",
	}, { // Test 1: No body at all.
		Name: "empty", Body: "",
	}, { // Test 2: Valid JSON that is not the error envelope.
		Name: "wrong json shape", Body: `{"detail":"nope"}`,
	}, { // Test 3: The envelope with an empty message, which carries nothing to fold.
		Name: "empty message", Body: `{"error":"   "}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			tr, _ := scriptedTransport(t, http.StatusBadGateway, test.Body)
			_, err := tr.Get(context.Background(), "run_x")
			if err == nil {
				t.Fatal("Get() answered nil on a 502")
			}
			if !strings.Contains(err.Error(), "502") {
				t.Errorf("Get() error = %q, want it to carry the status", err)
			}
			if strings.Contains(err.Error(), "<html>") {
				t.Errorf("Get() error = %q, want the proxy's markup kept out of it", err)
			}
		})
	}
}

// TestClaimRefusesARunBodyItCannotDecode pins that a truncated or malformed claim response fails the
// claim rather than handing the dispatcher a half-built run. A relay cut off mid-response is the
// ordinary shape of a dropped connection, and a worker that executed the zero value of a run would
// run nothing against nothing and then report an outcome for it.
func TestClaimRefusesARunBodyItCannotDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
	}{{ // Test 0: The body was cut off mid-object, which is a dropped connection.
		Name: "truncated", Body: `{"id":"run_1","playbook":"si`,
	}, { // Test 1: Not JSON at all, which is a proxy answering in the control node's place.
		Name: "not json", Body: "<html>hello</html>",
	}, { // Test 2: A JSON array where an object was owed.
		Name: "array", Body: `[{"id":"run_1"}]`,
	}, { // Test 3: An empty body under a 200, which says nothing at all.
		Name: "empty", Body: "",
	}, { // Test 4: A field of the wrong type inside an otherwise valid object.
		Name: "wrong field type", Body: `{"id":42}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			tr, _ := scriptedTransport(t, http.StatusOK, test.Body)
			got, err := tr.Claim(context.Background(), "worker-a", []string{""})
			if err == nil {
				t.Fatalf("Claim() accepted %s and returned %+v, so a worker executes a run the "+
					"control node never sent", test.Name, got)
			}
			if got != nil {
				t.Errorf("Claim() returned a run alongside an error, so a caller that ignores "+
					"the error runs %+v", got)
			}
		})
	}
}

// TestPoliciesRefusesABodyItCannotDecode pins that an unreadable policy list is an error, never an
// empty list. The plan-content gate runs where the run executes, so "there are no policies" and "I
// could not tell" have to stay distinguishable: reading the second as the first turns a gate that
// could not be evaluated into a gate that passed.
func TestPoliciesRefusesABodyItCannotDecode(t *testing.T) {
	t.Parallel()
	for testNum, body := range []string{`[{"id":`, "<html>", `{"id":"pol_1"}`, ""} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tr, _ := scriptedTransport(t, http.StatusOK, body)
			got, err := tr.Policies(context.Background())
			if err == nil {
				t.Fatalf("Policies() accepted %q and returned %v, so an unreadable answer reads "+
					"as an install that gates nothing", body, got)
			}
			if got != nil {
				t.Errorf("Policies() returned %v alongside an error", got)
			}
		})
	}
}

// TestProposeApplyDemandsACreatedResponse pins that only a 201 carrying a decodable run is read as a
// proposal. This is the one call that causes a run to exist, so a worker that read any other answer
// as success would carry on believing a gated apply was queued when nothing was created.
func TestProposeApplyDemandsACreatedResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Body    string
		Status  int
		WantErr bool
	}{{ // Test 0: A 201 with a decodable run is the proposal.
		Name: "created", Status: http.StatusCreated, Body: `{"id":"run_apply"}`, WantErr: false,
	}, { // Test 1: A 200 is not a creation, however healthy it looks.
		Name: "ok is not created", Status: http.StatusOK, Body: `{"id":"run_apply"}`, WantErr: true,
	}, { // Test 2: A 204 says nothing was created.
		Name: "no content", Status: http.StatusNoContent, Body: "", WantErr: true,
	}, { // Test 3: A rule refused the apply, which is an answer and not a proposal.
		Name: "conflict", Status: http.StatusConflict, Body: `{"error":"a rule holds this"}`,
		WantErr: true,
	}, { // Test 4: A 201 whose body cannot be decoded is not a proposal either.
		Name: "created but truncated", Status: http.StatusCreated, Body: `{"id":`, WantErr: true,
	}, { // Test 5: A 201 with no body at all.
		Name: "created but empty", Status: http.StatusCreated, Body: "", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			tr, _ := scriptedTransport(t, test.Status, test.Body)
			got, err := tr.ProposeApply(context.Background(), "run_plan", 3, true)
			if test.WantErr {
				if err == nil {
					t.Fatalf("ProposeApply() accepted %s and returned %+v", test.Name, got)
				}
				if got != nil {
					t.Errorf("ProposeApply() returned %+v alongside an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProposeApply() error = %v on a 201", err)
			}
			if diff := cmp.Diff("run_apply", got.ID); diff != "" {
				t.Errorf("proposal id mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClaimWithoutALeaseHeaderStillWorks pins that a control node predating the per-claim capability
// still hands out work. The capability rides in on a header, so an older control node simply sends
// none; a transport that demanded one would refuse every run in an install mid-upgrade, which is the
// exact moment the two sides disagree.
func TestClaimWithoutALeaseHeaderStillWorks(t *testing.T) {
	t.Parallel()
	tr, script := scriptedTransport(t, http.StatusOK, `{"id":"run_old","status":"running"}`)
	got, err := tr.Claim(context.Background(), "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v against a control node that sends no lease header", err)
	}
	if diff := cmp.Diff("run_old", got.ID); diff != "" {
		t.Errorf("claimed run mismatch (-want +got):\n%s", diff)
	}
	// A report for that run is still made, the older way, rather than being withheld for want of a
	// capability the control node never minted.
	if err := tr.Heartbeat(context.Background(), "run_old", "worker-a"); err == nil {
		t.Error("Heartbeat() answered nil against a 200, which is not the documented 204")
	}
	if script.seen("/heartbeat") != 1 {
		t.Errorf("heartbeat requests = %d, want 1", script.seen("/heartbeat"))
	}
}

// TestClaimNoContentIsAnIdlePollNotAFailure pins that an empty queue maps back to run.ErrNonePending.
// The claim loop polls constantly, so reading "nothing pending" as a fault would fill a worker's log
// with errors and, worse, could push a supervisor into restarting a healthy worker.
func TestClaimNoContentIsAnIdlePollNotAFailure(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, http.StatusNoContent, "")
	got, err := tr.Claim(context.Background(), "worker-a", nil)
	if !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() error = %v, want ErrNonePending", err)
	}
	if got != nil {
		t.Errorf("Claim() returned %+v with nothing pending", got)
	}
}

// TestNotFoundMapsBackToTheStoresSentinel pins that a run purged out from under a worker surfaces as
// run.ErrNotFound on the calls that can meet one, and as a plain error on the calls that cannot. The
// dispatcher branches on the sentinel to stop working a run that no longer exists, so a 404 arriving
// as an opaque error would keep it heartbeating a run that is gone.
func TestNotFoundMapsBackToTheStoresSentinel(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, http.StatusNotFound, `{"error":"run not found"}`)
	ctx := context.Background()

	if err := tr.Heartbeat(ctx, "run_gone", "worker-a"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() error = %v, want ErrNotFound", err)
	}
	if _, err := tr.Get(ctx, "run_gone"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
	// A 404 on the record writes is not the same sentinel: those endpoints answer 404 only when the
	// run is gone, and the caller treats the failure as a failed report rather than as a lost run.
	if err := tr.SaveHostSummary(ctx, "run_gone", []run.HostSummary{{Host: "web01"}}); err == nil {
		t.Error("SaveHostSummary() answered nil on a 404")
	}
}

// TestAReportBatchIsRetriedWhereItFailed pins how many times one report batch is posted before the
// call is reported as failed. The retry lives here rather than in the caller because a caller retry
// starts the whole sequence over, and the batches that had already landed get sent again, so a run's
// event record holds the same tasks twice.
func TestAReportBatchIsRetriedWhereItFailed(t *testing.T) {
	t.Parallel()
	tr, script := scriptedTransport(t, http.StatusInternalServerError, `{"error":"down"}`)
	if err := tr.SaveHostSummary(context.Background(), "run_1",
		[]run.HostSummary{{Host: "web01"}}); err == nil {
		t.Fatal("SaveHostSummary() answered nil against a relay answering 500")
	}
	got := script.seen("/host-summary")
	if got < 2 {
		t.Errorf("host summary posts = %d, want the batch retried rather than given up on at "+
			"the first failure", got)
	}
	if got > 8 {
		t.Errorf("host summary posts = %d, want a bounded retry budget rather than a hot loop", got)
	}
}

// TestReportsThatCarryNothingMakeNoCall pins that a run which reported nothing does not spend a
// request saying so. A quiet run is the common case at fleet scale, and a call per empty report is a
// request per run per report kind against a control node that has nothing to do with them.
func TestReportsThatCarryNothingMakeNoCall(t *testing.T) {
	t.Parallel()
	tr, script := scriptedTransport(t, http.StatusNoContent, "")
	ctx := context.Background()

	if err := tr.AppendEvents(ctx, "run_1", nil); err != nil {
		t.Errorf("AppendEvents(nil) error = %v", err)
	}
	if err := tr.SaveHostSummary(ctx, "run_1", nil); err != nil {
		t.Errorf("SaveHostSummary(nil) error = %v", err)
	}
	if err := tr.SaveHostFacts(ctx, "run_1", []run.HostFacts{}); err != nil {
		t.Errorf("SaveHostFacts(empty) error = %v", err)
	}
	if err := tr.SaveTaskSummary(ctx, "run_1", nil); err != nil {
		t.Errorf("SaveTaskSummary(nil) error = %v", err)
	}
	if err := tr.AppendLog(ctx, "run_1", nil); err != nil {
		t.Errorf("AppendLog(nil) error = %v", err)
	}
	if err := tr.AppendLog(ctx, "run_1", []byte{}); err != nil {
		t.Errorf("AppendLog(empty) error = %v", err)
	}
	if got := len(script.paths); got != 0 {
		t.Errorf("requests = %d for reports carrying nothing, want none: %v", got, script.paths)
	}
}

// TestRunIDSeparatorsCannotEscapeTheirPathSegment pins that a run id reaches the control node as the
// id it is, whatever it contains.
//
// The id lands in the URL a worker builds, so an unescaped separator in it would address a different
// route than the run it names. Ids are generated, but the escaping is what keeps that true rather
// than assumed: nothing else stands between a stored id and the path a worker constructs from it.
//
//nolint:funlen // Test function.
func TestRunIDSeparatorsCannotEscapeTheirPathSegment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil,
		nil))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	tests := []struct {
		Name      string
		ID        string
		WantReach bool
	}{{ // Test 0: The ordinary generated shape.
		Name: "plain", ID: "run_a1b2", WantReach: true,
	}, { // Test 1: A path separator stays inside one segment.
		Name: "separator", ID: "run/nested", WantReach: true,
	}, { // Test 2: A space is carried, not turned into another character.
		Name: "space", ID: "run with space", WantReach: true,
	}, { // Test 3: Non-ASCII survives as itself.
		Name: "non-ascii", ID: "run_日本", WantReach: true,
	}, { // Test 4: An id that already looks percent encoded is not decoded a second time.
		Name: "looks encoded", ID: "run%2Fnested", WantReach: true,
	}, { // Test 5: A query separator cannot open a query string.
		Name: "query separator", ID: "run?part=continue", WantReach: true,
	}, { // Test 6: A fragment separator cannot truncate the path.
		Name: "fragment separator", ID: "run#frag", WantReach: true,
	}, { // Test 7: A traversal does not reach a run and does not reach anything else either.
		Name: "traversal", ID: "..", WantReach: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			seeded := runningRun(test.ID)
			if err := backing.Save(ctx, seeded); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}
			got, err := c.Get(ctx, test.ID)
			if !test.WantReach {
				if err == nil {
					t.Fatalf("Get(%q) reached run %q, so a traversal in an id addresses "+
						"something", test.ID, got.ID)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get(%q) error = %v", test.ID, err)
			}
			if diff := cmp.Diff(test.ID, got.ID); diff != "" {
				t.Errorf("Get() reached the wrong run (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnIDCarryingTheContinuationMarkerIsStillAWholeReport pins that escaping protects the report
// framing, not only the routing. The continuation marker is a query parameter, and a report endpoint
// reads it to decide whether to add to what a run has stored or replace it. If a run id carrying that
// text leaked out of its path segment, the first batch of a report would upsert onto a stale set
// instead of clearing it, and the run's stored summary would be a mix of two attempts.
func TestAnIDCarryingTheContinuationMarkerIsStillAWholeReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil,
		nil))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	const id = "run_x?part=continue"
	if err := backing.Save(ctx, runningRun(id)); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, id, []run.HostSummary{{Host: "first"}}); err != nil {
		t.Fatalf("SaveHostSummary(first) error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, id, []run.HostSummary{{Host: "second"}}); err != nil {
		t.Fatalf("SaveHostSummary(second) error = %v", err)
	}
	got, err := backing.RunHostSummaries(ctx, id)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	want := []run.HostSummary{{RunID: id, Host: "second"}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a whole report did not replace the one before it, so a marker inside the id was "+
			"read as a continuation (-want +got):\n%s", diff)
	}
}
