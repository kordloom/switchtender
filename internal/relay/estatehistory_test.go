package relay_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAWorkerGatherBecomesEstateHistory covers the estate against distributed workers.
//
// A worker holds no database. Everything it learns travels to the control node over the relay, so
// a reading gathered on a worker becomes estate history only if that path carries it and the
// control node stores it the same way it stores its own. Distributed execution is the Team feature,
// so a fleet large enough to need workers is exactly the fleet whose history would be missing.
func TestAWorkerGatherBecomesEstateHistory(t *testing.T) {
	t.Parallel()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)
	worker := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	// The runs exist on the control node first: a worker reports against a run it was leased, and
	// the relay refuses facts for a run it has never heard of.
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"run_1", "run_2"} {
		if err := backing.Save(ctx, &run.Run{ID: id, Playbook: "site.yml",
			Status: run.StatusPending, CreatedAt: at}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	// Claimed through the relay, because a worker may only report on a run it was leased. Reporting
	// facts for a run nobody handed it is refused, which is the guard this has to satisfy rather
	// than route around.
	report := func(kernel string, gathered time.Time) {
		t.Helper()
		leased, err := worker.Claim(ctx, "worker-a", []string{""})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if leased == nil {
			t.Fatal("nothing was leased, so there is no run to report against")
		}
		// The per host result first. Facts for a host the run never touched are refused, which
		// stops a worker asserting the state of a machine it was never given.
		if err := worker.SaveHostSummary(ctx, leased.ID, []run.HostSummary{{
			Host: "web01", OK: 1, Worst: "ok", RanAt: gathered,
		}}); err != nil {
			t.Fatalf("a worker could not report its per host result: %v", err)
		}
		if err := worker.SaveHostFacts(ctx, leased.ID, []run.HostFacts{{
			Host: "web01", Facts: map[string]string{"kernel": kernel}, GatheredAt: gathered,
		}}); err != nil {
			t.Fatalf("a worker could not report what it gathered: %v", err)
		}
	}
	later := at.AddDate(0, 1, 0)
	report("5.15.0", at)
	report("6.8.0", later)

	// The control node answers the estate from what the worker sent, at both points.
	was, err := backing.EstateAt(ctx, at.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt: %v", err)
	}
	if len(was) != 1 || was[0].Facts["kernel"] != "5.15.0" {
		t.Fatalf("the estate in March is %+v, want web01 on the kernel the worker reported then. "+
			"A worker's gather is not becoming history, so a fleet large enough to need workers "+
			"has no estate at all", was)
	}
	now, err := backing.EstateAt(ctx, later.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt: %v", err)
	}
	if len(now) != 1 || now[0].Facts["kernel"] != "6.8.0" {
		t.Errorf("the estate later is %+v, want the newer reading the worker sent", now)
	}

	// A worker never decides who may read anything, so the reads themselves stay on the control
	// node rather than being served to it.
	if _, err := worker.EstateAt(ctx, at, 0); err == nil {
		t.Error("a worker was served an estate read, which is a control-node question")
	}
}
