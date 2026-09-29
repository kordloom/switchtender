package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// hitCounter records every request a notification channel makes, with its path and body.
type hitCounter struct {
	// mu guards paths and bodies.
	mu sync.Mutex
	// paths lists the request paths in arrival order.
	paths []string
	// bodies lists the request bodies in arrival order.
	bodies []string
}

// record stores one delivery.
func (h *hitCounter) record(path, body string) {
	h.mu.Lock()
	h.paths = append(h.paths, path)
	h.bodies = append(h.bodies, body)
	h.mu.Unlock()
}

// count returns how many deliveries arrived.
func (h *hitCounter) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.paths)
}

// allBodies returns every delivered body joined, for a substring search across the lot.
func (h *hitCounter) allBodies() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.bodies, "\n")
}

// notifyServer starts a receiver that accepts every notification and records it.
func notifyServer(t *testing.T) (*httptest.Server, *hitCounter) {
	t.Helper()
	hits := &hitCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		hits.record(req.URL.Path, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// notifyDispatcher builds the smallest dispatcher the per-run notification path needs, pointed at a
// receiver on loopback, which the default guarded client would otherwise refuse.
func notifyDispatcher(client *http.Client) *Dispatcher {
	return &Dispatcher{log: zap.NewNop(), notifyHTTP: client}
}

// TestNotifyRunTargetsIsBoundedByTheLimit pins the ceiling on how far one run can fan out. Each
// target costs a goroutine and a socket to an address whoever started the run chose, so a run stored
// before the limit existed, or one that got past the write-side check, must still be bounded here.
// The excess is dropped and named, because delivering to some of a list while reporting success reads
// as having delivered to all of it.
func TestNotifyRunTargetsIsBoundedByTheLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says how many targets the run carries.
		Name string
		// Targets is how many webhook targets are stored on the run.
		Targets int
		// WantDeliveries is how many requests may leave the host.
		WantDeliveries int
	}{{ // Test 0: A run carrying nothing delivers nothing.
		Name: "no targets", Targets: 0, WantDeliveries: 0,
	}, { // Test 1: One target delivers once.
		Name: "one target", Targets: 1, WantDeliveries: 1,
	}, { // Test 2: Exactly the limit is delivered in full.
		Name: "exactly the limit", Targets: run.MaxNotifyTargets,
		WantDeliveries: run.MaxNotifyTargets,
	}, { // Test 3: One past the limit is truncated to the limit.
		Name: "one past the limit", Targets: run.MaxNotifyTargets + 1,
		WantDeliveries: run.MaxNotifyTargets,
	}, { // Test 4: A run far past the limit is still bounded to it.
		Name: "far past the limit", Targets: run.MaxNotifyTargets * 4,
		WantDeliveries: run.MaxNotifyTargets,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv, hits := notifyServer(t)
			d := notifyDispatcher(srv.Client())

			r := &run.Run{
				ID: "run_fanout", Playbook: "site.yml", Status: run.StatusSucceeded,
				CreatedAt: time.Now(),
			}
			for i := range test.Targets {
				r.Notifications = append(r.Notifications, run.NotifyTarget{
					Kind: run.NotifyWebhook, URL: fmt.Sprintf("%s/hook/%d", srv.URL, i),
				})
			}

			d.notifyRunTargets(r)
			d.notifyWG.Wait()

			if got := hits.count(); got != test.WantDeliveries {
				t.Errorf("%d deliveries from a run carrying %d targets, want %d: the fan-out is "+
					"one goroutine and one socket per target to an address whoever started the run "+
					"chose", got, test.Targets, test.WantDeliveries)
			}
		})
	}
}

// TestNotifyRunTargetsRefusesWhatItShouldNotSend walks the per-target filters. An unknown kind has no
// formatter, so sending anything for it would be guessing; a URL-configured kind with no URL has
// nowhere to go; and a failure-only target must stay quiet on a green run, which is the whole reason
// somebody sets it.
func TestNotifyRunTargetsRefusesWhatItShouldNotSend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which target is being filtered.
		Name string
		// Status is the run's terminal status.
		Status run.Status
		// Target is the single target the run carries.
		Target func(base string) run.NotifyTarget
		// WantDeliveries is how many requests should leave the host.
		WantDeliveries int
	}{{ // Test 0: A kind nothing knows how to format is skipped.
		Name: "an unknown kind", Status: run.StatusFailed,
		Target: func(base string) run.NotifyTarget {
			return run.NotifyTarget{Kind: "carrier-pigeon", URL: base + "/hook"}
		},
		WantDeliveries: 0,
	}, { // Test 1: An empty kind is likewise nothing to route by.
		Name: "an empty kind", Status: run.StatusFailed,
		Target: func(base string) run.NotifyTarget {
			return run.NotifyTarget{Kind: "", URL: base + "/hook"}
		},
		WantDeliveries: 0,
	}, { // Test 2: A URL-configured kind with no URL has nowhere to deliver.
		Name: "a webhook with no url", Status: run.StatusFailed,
		Target:         func(string) run.NotifyTarget { return run.NotifyTarget{Kind: run.NotifyWebhook} },
		WantDeliveries: 0,
	}, { // Test 3: A failure-only target stays quiet on a run that succeeded.
		Name: "failure-only on a green run", Status: run.StatusSucceeded,
		Target: func(base string) run.NotifyTarget {
			return run.NotifyTarget{Kind: run.NotifyWebhook, URL: base + "/hook", OnFailure: true}
		},
		WantDeliveries: 0,
	}, { // Test 4: The same target fires on a failure, which is what it was set for.
		Name: "failure-only on a failed run", Status: run.StatusFailed,
		Target: func(base string) run.NotifyTarget {
			return run.NotifyTarget{Kind: run.NotifyWebhook, URL: base + "/hook", OnFailure: true}
		},
		WantDeliveries: 1,
	}, { // Test 5: And on an interrupted run, which is trouble too.
		Name: "failure-only on an interrupted run", Status: run.StatusInterrupted,
		Target: func(base string) run.NotifyTarget {
			return run.NotifyTarget{Kind: run.NotifyWebhook, URL: base + "/hook", OnFailure: true}
		},
		WantDeliveries: 1,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv, hits := notifyServer(t)
			d := notifyDispatcher(srv.Client())

			r := &run.Run{
				ID: "run_filter", Playbook: "site.yml", Status: test.Status, CreatedAt: time.Now(),
				Notifications: []run.NotifyTarget{test.Target(srv.URL)},
			}
			d.notifyRunTargets(r)
			d.notifyWG.Wait()

			if got := hits.count(); got != test.WantDeliveries {
				t.Errorf("%d deliveries, want %d", got, test.WantDeliveries)
			}
		})
	}
}

// TestNotifyRunTargetsDeliversEveryUrlChannel proves the filters above are not simply refusing
// everything. Each URL-configured kind must reach its own formatter, since a Discord webhook fed a
// Slack payload is rejected by the service and the operator sees silence.
func TestNotifyRunTargetsDeliversEveryUrlChannel(t *testing.T) {
	t.Parallel()
	srv, hits := notifyServer(t)
	d := notifyDispatcher(srv.Client())

	kinds := []string{
		run.NotifyWebhook, run.NotifySlack, run.NotifyMattermost, run.NotifyRocketChat,
		run.NotifyDiscord, run.NotifyTeams, run.NotifyNtfy,
	}
	r := &run.Run{
		ID: "run_all_kinds", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: time.Now(),
	}
	for _, kind := range kinds {
		r.Notifications = append(r.Notifications, run.NotifyTarget{
			Kind: kind, URL: srv.URL + "/" + kind,
		})
	}

	d.notifyRunTargets(r)
	d.notifyWG.Wait()

	if got := hits.count(); got != len(kinds) {
		t.Fatalf("%d deliveries, want one per kind (%d)", got, len(kinds))
	}
	hits.mu.Lock()
	got := append([]string(nil), hits.paths...)
	hits.mu.Unlock()
	want := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		want = append(want, "/"+kind)
	}
	less := func(a, b string) bool { return a < b }
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(less), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("delivered paths (-want +got):\n%s", diff)
	}
}

// TestAPerRunWebhookNeverCarriesTheRunSecrets pins what leaves the host on the per-run path. A
// notification target is named by whoever started the run, so the payload reaches an address the
// operator did not choose. Extra vars carry survey answers, Command is the run's raw script body,
// Outputs is where a fetched token re-enters a run, and the target list itself carries routing keys
// and API tokens. All four must be stripped, and the same is true of a pipeline step's script.
func TestAPerRunWebhookNeverCarriesTheRunSecrets(t *testing.T) {
	t.Parallel()
	srv, hits := notifyServer(t)
	d := notifyDispatcher(srv.Client())

	r := &run.Run{
		ID: "run_leaky", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: time.Now(),
		Command:   "export DB_PASSWORD=survey-answer-secret && deploy",
		ExtraVars: map[string]any{"db_password": "extra-var-secret"},
		Outputs:   map[string]any{"fetched_token": "outputs-secret"},
		Steps: []run.PipelineStep{
			{Name: "one", Tool: run.ToolBash, Command: "echo step-body-secret"},
		},
		Notifications: []run.NotifyTarget{
			{Kind: run.NotifyWebhook, URL: srv.URL + "/hook"},
			{Kind: run.NotifyPagerDuty, Key: "routing-key-secret"},
		},
	}

	d.notifyRunTargets(r)
	d.notifyWG.Wait()

	body := hits.allBodies()
	if body == "" {
		t.Fatal("nothing was delivered, so this proves nothing")
	}
	for _, secret := range []string{
		"survey-answer-secret", "extra-var-secret", "outputs-secret", "step-body-secret",
		"routing-key-secret",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("the delivered payload carries %q off the host to an address the run named",
				secret)
		}
	}

	// The run itself keeps everything, so only what leaves the host is stripped.
	if r.Command == "" || len(r.ExtraVars) == 0 || len(r.Outputs) == 0 {
		t.Error("redacting for an external channel mutated the caller's run, so the in-tenant " +
			"record lost fields the interface shows")
	}
	if len(r.Steps) != 1 || r.Steps[0].Command == "" {
		t.Error("a pipeline step's script was blanked on the caller's own run")
	}
}

// TestRedactForExternalKeepsTheRunReadable is the other half of the same rule: stripping the secret
// fields must leave a notification that still identifies the run. A payload with no id or status is
// useless to the channel receiving it.
func TestRedactForExternalKeepsTheRunReadable(t *testing.T) {
	t.Parallel()
	ended := time.Now()
	code := 2
	r := &run.Run{
		ID: "run_readable", Playbook: "site.yml", Inventory: "prod", Status: run.StatusFailed,
		CreatedAt: ended, EndedAt: &ended, ExitCode: &code, Actor: "casey",
		Command:   "secret",
		ExtraVars: map[string]any{"a": "b"},
		Steps:     []run.PipelineStep{{Name: "build", Tool: run.ToolBash, Command: "secret"}},
	}

	out := redactForExternal(r)

	if out.ID != r.ID || out.Status != r.Status || out.Playbook != r.Playbook {
		t.Errorf("the redacted run lost its identity: %+v", out)
	}
	if out.ExitCode == nil || *out.ExitCode != code {
		t.Errorf("ExitCode = %v, want the code preserved", out.ExitCode)
	}
	if out.Command != "" || out.ExtraVars != nil || out.Outputs != nil || out.Notifications != nil {
		t.Errorf("a secret-bearing field survived redaction: %+v", out)
	}
	if len(out.Steps) != 1 {
		t.Fatalf("steps = %d, want the pipeline shape preserved", len(out.Steps))
	}
	if out.Steps[0].Command != "" {
		t.Errorf("step command = %q, want it blanked", out.Steps[0].Command)
	}
	if out.Steps[0].Name != "build" || out.Steps[0].Tool != run.ToolBash {
		t.Errorf("the step lost its non-secret shape: %+v", out.Steps[0])
	}

	// The payload still encodes, which is what the webhook path does with it next.
	if _, err := json.Marshal(notification{Event: "run.finished", Run: &out}); err != nil {
		t.Errorf("the redacted run no longer encodes: %v", err)
	}
}

// TestNotifySkipsChildrenAndUnfinishedRuns pins the two guards at the top of the notify fan-out. A
// shard or step notifying on its own would page once per child instead of once per run, and notifying
// before a run is terminal would announce an outcome it has not reached.
func TestNotifySkipsChildrenAndUnfinishedRuns(t *testing.T) {
	t.Parallel()

	parentID := "run_parent"
	tests := []struct {
		// Name says which run is being offered to the fan-out.
		Name string
		// Run is that run.
		Run *run.Run
		// WantDeliveries is how many requests should leave the host.
		WantDeliveries int
	}{{ // Test 0: A shard notifies through its parent, never on its own.
		Name:           "a shard",
		Run:            &run.Run{ID: "run_shard", Status: run.StatusFailed, ParentID: &parentID},
		WantDeliveries: 0,
	}, { // Test 1: A run still executing has reached no outcome to announce.
		Name:           "still running",
		Run:            &run.Run{ID: "run_live", Status: run.StatusRunning},
		WantDeliveries: 0,
	}, { // Test 2: A run still awaiting a decision likewise.
		Name:           "awaiting approval",
		Run:            &run.Run{ID: "run_held", Status: run.StatusPendingApproval},
		WantDeliveries: 0,
	}, { // Test 3: A finished top-level run is the one that notifies.
		Name:           "a finished top-level run",
		Run:            &run.Run{ID: "run_done", Status: run.StatusFailed},
		WantDeliveries: 1,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv, hits := notifyServer(t)
			d := notifyDispatcher(srv.Client())
			d.webhooks = []string{srv.URL + "/hook"}

			test.Run.CreatedAt = time.Now()
			d.notify(test.Run)
			d.notifyWG.Wait()

			if got := hits.count(); got != test.WantDeliveries {
				t.Errorf("%d deliveries, want %d", got, test.WantDeliveries)
			}
		})
	}
}

// TestNotifyRicherTargetsSkipsUnconfiguredTransports pins that a target naming a transport the server
// does not have is skipped rather than panicking on a nil emailer or sending through a blank Twilio
// account. Twilio and email carry only a recipient, because their credentials are server-held, so a
// target can outlive the configuration that made it sendable.
func TestNotifyRicherTargetsSkipsUnconfiguredTransports(t *testing.T) {
	t.Parallel()
	d := notifyDispatcher(http.DefaultClient)

	r := &run.Run{
		ID: "run_unconfigured", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: time.Now(),
	}
	d.notifyRicherTargets(r, []run.NotifyTarget{
		{Kind: run.NotifyTwilio, To: "+15550000000"},
		{Kind: run.NotifyEmail, To: "ops@example.com"},
		{Kind: run.NotifyEmail, To: "   ,  , "},
	})
	d.notifyWG.Wait()
}

// TestNotifyRicherTargetsHoldsPagerDutyToFailures pins the one channel that must stay quiet on a green
// run. PagerDuty opens an incident, so triggering one for every successful run would page a person
// each time a change worked.
func TestNotifyRicherTargetsHoldsPagerDutyToFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Status is the run's terminal status.
		Status run.Status
		// WantIncidents is how many incidents may be opened.
		WantIncidents int
	}{
		{Status: run.StatusSucceeded, WantIncidents: 0},   // Test 0: A green run pages nobody.
		{Status: run.StatusCanceled, WantIncidents: 0},    // Test 1: A cancel is somebody's decision.
		{Status: run.StatusRejected, WantIncidents: 0},    // Test 2: A rejection likewise.
		{Status: run.StatusFailed, WantIncidents: 1},      // Test 3: A failure opens one.
		{Status: run.StatusInterrupted, WantIncidents: 1}, // Test 4: So does a lost server.
	}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv, hits := notifyServer(t)
			d := notifyDispatcher(srv.Client())
			d.pagerDutyEndpoint = srv.URL + "/v2/enqueue"

			d.notifyRicherTargets(&run.Run{
				ID: "run_pd", Playbook: "site.yml", Status: test.Status, CreatedAt: time.Now(),
				Error: "something broke",
			}, []run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "rk"}})
			d.notifyWG.Wait()

			if got := hits.count(); got != test.WantIncidents {
				t.Errorf("%d incidents for a %q run, want %d", got, test.Status, test.WantIncidents)
			}
		})
	}
}

// TestDeliverStopsRetryingAfterTwoAttempts pins the bound on one delivery. A notification target is an
// address whoever started the run chose, so an endpoint that always refuses must cost a fixed amount
// of work rather than retrying indefinitely and holding a shutdown open.
func TestDeliverStopsRetryingAfterTwoAttempts(t *testing.T) {
	t.Parallel()
	hits := &hitCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		hits.record(req.URL.Path, string(body))
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := notifyDispatcher(srv.Client())
	start := time.Now()
	d.deliver(srv.URL+"/hook", "run_retry", []byte(`{"event":"run.finished"}`))

	if got := hits.count(); got != 2 {
		t.Errorf("%d attempts against an endpoint that always refuses, want 2", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("one delivery took %v, which holds a shutdown open per target", elapsed)
	}
}
