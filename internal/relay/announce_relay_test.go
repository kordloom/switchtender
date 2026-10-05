package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// announced is the part of a webhook body these tests read.
type announced struct {
	// Event is run.finished or run.held.
	Event string `json:"event"`
	// Run carries the id and status of the run announced.
	Run struct {
		// ID is the run's id.
		ID string `json:"id"`
		// Status is the run's status when it was announced.
		Status run.Status `json:"status"`
	} `json:"run"`
}

// TestTheControlNodeAnnouncesWhatWorkersDo drives a worker over the relay against a control node
// whose webhook is listening, and requires the webhook to hear about the worker's runs.
//
// A worker holds no notification channels, and the relay announced nothing, so every channel an
// operator configured heard about the runs the control node executed and not one a worker did. An
// apply held on a worker's plan waited for a person nobody told. The control node now announces
// both, at the moment it records them.
func TestTheControlNodeAnnouncesWhatWorkersDo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the worker does.
		Name string
		// Run is the run seeded for the worker to claim.
		Run *run.Run
		// Output is what the worker's tool prints.
		Output string
		// WantEvents are the announcements the webhook must hear, keyed by event, each naming a
		// run's status. The plan's own id is filled in for the finished plan.
		WantEvents map[string]run.Status
	}{{ // Test 0: A run the worker finishes is announced as finished.
		Name: "finished run",
		Run: &run.Run{ID: "run_worker_bash", Tool: run.ToolBash, Command: "echo hi",
			Status: run.StatusPending, CreatedAt: time.Now()},
		Output:     "hi\n",
		WantEvents: map[string]run.Status{"run.finished": run.StatusSucceeded},
	}, { // Test 1: An apply held on the worker's plan is announced as held, beside the plan's finish.
		Name: "held apply",
		Run: &run.Run{ID: "run_worker_plan", Tool: run.ToolTerraform, Command: "infra/prod",
			Status: run.StatusPending, CreatedAt: time.Now()},
		Output: "Plan: 0 to add, 0 to change, 3 to destroy.\n",
		WantEvents: map[string]run.Status{
			"run.finished": run.StatusSucceeded, "run.held": run.StatusPendingApproval,
		},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			events := make(chan announced, 8)
			hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var e announced
				if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
					events <- e
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(hook.Close)

			backing := run.NewMemStore()
			// The control node executes nothing here, so every run is the worker's.
			control := dispatch.New(backing, roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{}, errors.New("the control node executes nothing here")
				}), zaptest.NewLogger(t), dispatch.WithNoJanitor(),
				dispatch.WithWebhooks([]string{hook.URL}), dispatch.WithNotifyClient(http.DefaultClient),
				dispatch.WithClaimGate(func() error { return errors.New("closed for this test") }))
			t.Cleanup(control.Close)
			ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil,
				planGateRules(t), nil, relay.WithPlanSealer(planSealerStub{}),
				relay.WithAnnouncer(control)))
			t.Cleanup(ts.Close)

			transport := relay.NewHTTPTransport(ts.URL, testWorkerToken, nil)
			tool := roundhouse.RunnerFunc(
				func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					_, err := io.WriteString(out, test.Output)
					if spec.DryRun {
						return plantest.Result(test.Output), err
					}
					return roundhouse.Result{ExitCode: 0}, err
				})
			worker := dispatch.New(relay.NewClient(transport), tool, zaptest.NewLogger(t),
				dispatch.WithPolicies(relay.NewPolicyClient(transport)), dispatch.WithWorkers(1),
				dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
				dispatch.WithClaimInterval(10*time.Millisecond))
			t.Cleanup(worker.Close)

			if err := backing.Save(ctx, test.Run); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}

			got := map[string]announced{}
			deadline := time.After(15 * time.Second)
			for len(got) < len(test.WantEvents) {
				select {
				case e := <-events:
					if _, seen := got[e.Event]; seen {
						t.Errorf("%s was announced twice: %+v", e.Event, e)
					}
					got[e.Event] = e
				case <-deadline:
					t.Fatalf("the webhook heard %v, want %v", got, test.WantEvents)
				}
			}
			for event, status := range test.WantEvents {
				if e := got[event]; e.Run.Status != status {
					t.Errorf("%s announced status %q, want %q", event, e.Run.Status, status)
				}
			}
			if e, ok := got["run.finished"]; ok && e.Run.ID != test.Run.ID {
				t.Errorf("the finished announcement names %s, want the worker's run %s",
					e.Run.ID, test.Run.ID)
			}
			select {
			case e := <-events:
				t.Errorf("an extra announcement arrived: %+v", e)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// TestTheRelayAnnouncesAHeldProposalOnce pins how often a proposal is announced. A worker whose
// answer never arrived reports again and gets the same proposal back, and announcing it again would
// ask the approver the same question twice. A queued apply waits for nobody, so nothing is said
// until it finishes.
func TestTheRelayAnnouncesAHeldProposalOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the plan found.
		Name string
		// Destroys is the count the worker reports.
		Destroys int
		// WantStatus is the proposal's status.
		WantStatus run.Status
		// WantAnnounced is how many announcements the two reports may make.
		WantAnnounced int
	}{{ // Test 0: A held apply is announced once however many times the worker reports.
		Name: "held", Destroys: 3, WantStatus: run.StatusPendingApproval, WantAnnounced: 1,
	}, { // Test 1: A queued apply is not announced at all.
		Name: "queued", Destroys: 0, WantStatus: run.StatusPending,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			backing := run.NewMemStore()
			var mu sync.Mutex
			var said []*run.Run
			announcer := relay.AnnouncerFunc(func(r *run.Run) {
				mu.Lock()
				defer mu.Unlock()
				said = append(said, r.Clone())
			})
			ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil,
				planGateRules(t), nil, relay.WithPlanSealer(planSealerStub{}),
				relay.WithAnnouncer(announcer)))
			t.Cleanup(ts.Close)

			lease := seedPlan(t, backing, "run_plan", nil)
			body := fmt.Sprintf(`{"destroys":%d,"read":true,"plan_file":"cGxhbg=="}`, test.Destroys)
			var proposal run.Run
			for range 2 {
				status, resp := leasedPost(t, ts.URL, "/relay/v1/runs/run_plan/propose-apply", lease,
					body)
				if status != http.StatusCreated {
					t.Fatalf("propose answered %d (%s), want 201", status, resp)
				}
				if err := json.Unmarshal([]byte(resp), &proposal); err != nil {
					t.Fatalf("decode proposal: %v", err)
				}
			}
			if proposal.Status != test.WantStatus {
				t.Errorf("proposal status = %q, want %q", proposal.Status, test.WantStatus)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(said) != test.WantAnnounced {
				t.Fatalf("announced %d times, want %d", len(said), test.WantAnnounced)
			}
			if len(said) > 0 && said[0].ID != proposal.ID {
				t.Errorf("announced %s, want the proposal %s", said[0].ID, proposal.ID)
			}
		})
	}
}

// TestTheControlNodeAnnouncesAWorkersStart pins that a run a worker executes reaches the named
// notification targets attached for its start, not only its finish. The control node hears the
// start through the relay's start endpoint, since the worker holds no targets of its own.
func TestTheControlNodeAnnouncesAWorkersStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := make(chan announced, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e announced
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			events <- e
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)
	router := dispatch.NotificationRouterFunc(func(context.Context, *run.Run) []run.NotifyTarget {
		return []run.NotifyTarget{{Kind: run.NotifyWebhook, URL: hook.URL}}
	})

	backing := run.NewMemStore()
	control := dispatch.New(backing, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, errors.New("the control node executes nothing here")
		}), zaptest.NewLogger(t), dispatch.WithNoJanitor(),
		dispatch.WithNotificationRouter(router), dispatch.WithNotifyClient(http.DefaultClient),
		dispatch.WithClaimGate(func() error { return errors.New("closed for this test") }))
	t.Cleanup(control.Close)
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil,
		planGateRules(t), nil, relay.WithPlanSealer(planSealerStub{}),
		relay.WithAnnouncer(control)))
	t.Cleanup(ts.Close)

	transport := relay.NewHTTPTransport(ts.URL, testWorkerToken, nil)
	tool := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, err := io.WriteString(out, "hi\n")
			return roundhouse.Result{ExitCode: 0}, err
		})
	worker := dispatch.New(relay.NewClient(transport), tool, zaptest.NewLogger(t),
		dispatch.WithPolicies(relay.NewPolicyClient(transport)), dispatch.WithWorkers(1),
		dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
		dispatch.WithClaimInterval(10*time.Millisecond))
	t.Cleanup(worker.Close)

	if err := backing.Save(ctx, &run.Run{ID: "run_worker_start", Tool: run.ToolBash,
		Command: "echo hi", Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	want := map[string]run.Status{"run.started": run.StatusRunning,
		"run.finished": run.StatusSucceeded}
	got := map[string]run.Status{}
	deadline := time.After(15 * time.Second)
	for len(got) < len(want) {
		select {
		case e := <-events:
			if _, seen := got[e.Event]; seen {
				t.Errorf("%s was announced twice", e.Event)
			}
			got[e.Event] = e.Run.Status
		case <-deadline:
			t.Fatalf("the targets heard %v, want %v", got, want)
		}
	}
	for event, status := range want {
		if got[event] != status {
			t.Errorf("%s announced status %q, want %q", event, got[event], status)
		}
	}
}
