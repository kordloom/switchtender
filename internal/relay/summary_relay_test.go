package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// recapRunner is a runner that writes the event stream a real playbook produces: a task, one result
// per host, and the closing recap that carries the per-host counters and the values the playbook
// published with set_stats.
type recapRunner struct{}

// Run writes the recap event lines to the run's sidecar and exits zero.
func (recapRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	if _, err := io.WriteString(out, "PLAY RECAP\n"); err != nil {
		return roundhouse.Result{}, err
	}
	lines := strings.Join([]string{
		`{"type":"task_start","ts":1719000000,"task":"install"}`,
		`{"type":"runner_ok","ts":1719000001,"task":"install","host":"web01"}`,
		`{"type":"runner_failed","ts":1719000002,"task":"install","host":"web02"}`,
		`{"type":"facts","ts":1719000002,"host":"web01","facts":{"distribution":"Debian"}}`,
		`{"type":"stats","ts":1719000003,"stats":{` +
			`"web01":{"ok":1,"changed":1,"failures":0,"unreachable":0,"skipped":0},` +
			`"web02":{"ok":0,"changed":0,"failures":1,"unreachable":0,"skipped":0}},` +
			`"outputs":{"artifact":"build-42"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(spec.EventsPath, []byte(lines), 0o600); err != nil {
		return roundhouse.Result{}, err
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestRelayRunRecordsHostsInOutcome drives the real relay path end to end: a worker dispatcher whose
// only store is a relay Client claims a run over HTTP, executes it, and streams its evidence back to
// the control node, which commits the outcome. Nothing in the test hands the control node a host
// summary; the run's own recap is the only source, exactly as in production. The committed outcome
// must therefore carry the run's hosts, and the digest the chain holds must verify against that body.
func TestRelayRunRecordsHostsInOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)

	client := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, nil))
	worker := dispatch.New(client, recapRunner{}, zaptest.NewLogger(t),
		dispatch.WithWorkers(1), dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
		dispatch.WithClaimInterval(10*time.Millisecond))
	t.Cleanup(worker.Close)

	if err := backing.Save(ctx, &run.Run{
		ID: "run_relay_hosts", Playbook: "site.yml", Inventory: "prod",
		Status: run.StatusPending, Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	finished := waitTerminal(t, backing, "run_relay_hosts")
	if finished.Status != run.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded (error %q)", finished.Status, finished.Error)
	}

	body, err := outcome.Body(ctx, backing, finished)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	var record outcome.Record
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatalf("unmarshal outcome body: %v", err)
	}
	want := []outcome.RecordHost{
		{Host: "web01", Worst: "changed", OK: 1, Changed: 1},
		{Host: "web02", Worst: "failed", Failures: 1},
	}
	if diff := cmp.Diff(want, record.Hosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("committed outcome hosts mismatch (-want +got):\n%s", diff)
	}
	wantTasks := []outcome.RecordTask{{Task: "install", Milliseconds: 2000}}
	if diff := cmp.Diff(wantTasks, record.Tasks, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("committed outcome tasks mismatch (-want +got):\n%s", diff)
	}

	entry := outcomeEntry(t, audits, "run_relay_hosts")
	if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, body) {
		t.Error("the committed digest does not verify against the outcome body carrying the hosts")
	}

	// The facts the run gathered cross the relay too, so a host a worker ran against is known to the
	// control node's inventory rather than dropped on the far side of the boundary.
	facts, err := backing.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if diff := cmp.Diff(map[string]string{"distribution": "Debian"}, facts.Facts); diff != "" {
		t.Errorf("stored host facts mismatch (-want +got):\n%s", diff)
	}

	// The values the run published come back on its record, which is what a pipeline step's
	// dependents read.
	if diff := cmp.Diff(map[string]any{"artifact": "build-42"}, finished.Outputs); diff != "" {
		t.Errorf("stored run outputs mismatch (-want +got):\n%s", diff)
	}
	if finished.Warning != "" {
		t.Errorf("run warning = %q, want none for a run that recorded its hosts", finished.Warning)
	}
}

// TestRelayRunWithoutRecapWarns proves a relay-executed run that produced no per-host result says so
// on its own record instead of quietly committing an outcome with no hosts. The record is what an
// operator reads when fleet health, drift, and host history show nothing for the run.
func TestRelayRunWithoutRecapWarns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)

	client := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, nil))
	silent := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	worker := dispatch.New(client, silent, zaptest.NewLogger(t),
		dispatch.WithWorkers(1), dispatch.WithNoJanitor(), dispatch.WithOwner("worker-b"),
		dispatch.WithClaimInterval(10*time.Millisecond))
	t.Cleanup(worker.Close)

	if err := backing.Save(ctx, &run.Run{
		ID: "run_relay_norecap", Playbook: "site.yml", Inventory: "prod",
		Status: run.StatusPending, Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	finished := waitTerminal(t, backing, "run_relay_norecap")
	if !strings.Contains(finished.Warning, "no per-host result") {
		t.Errorf("run warning = %q, want it to say the run recorded no per-host result",
			finished.Warning)
	}
	summaries, err := backing.RunHostSummaries(ctx, "run_relay_norecap")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("host summaries = %v, want none for a run with no recap", summaries)
	}
}

// waitTerminal polls the control node's store until the run reaches a terminal status.
func waitTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), id)
		if err == nil && got.Status.Terminal() {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a terminal status", id)
	return nil
}

// outcomeEntry returns the run outcome entry the control node committed for the run.
func outcomeEntry(t *testing.T, audits audit.Store, id string) *audit.Entry {
	t.Helper()
	// Waited for rather than read once. The run reaching a terminal state and its outcome reaching the
	// chain are two writes, and over the relay the second is the control node's, so a single read taken
	// the moment the status settles can arrive in the window between them. That window is nothing on an
	// idle machine and real when the whole suite is running under the race detector, which is exactly
	// when a gate must not report a problem the product does not have.
	prefix := fmt.Sprintf("/runs/%s/outcome/", id)
	deadline := time.Now().Add(15 * time.Second)
	for {
		chain, err := audits.Chain(context.Background())
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		for _, e := range chain {
			if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, prefix) {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no outcome entry committed for run %s", id)
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}
