package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRelaunchFailedHosts proves a relaunch re-runs the same spec against only the hosts that
// failed or were unreachable, links back to the source run, and refuses a run with no failed hosts
// or no per-host results. The link is what lets an auditor see which run's failures this one was
// built to fix, which is the edge over a plain re-run.
func TestRelaunchFailedHosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	// Host summaries are written while a run executes, before it terminalizes, so the run is saved
	// running first, then its summaries, then finished, exactly as the real dispatcher does.
	src := &run.Run{
		ID: "run_src", Playbook: "site.yml", Inventory: "hosts.ini", Tool: "ansible",
		Tags: []string{"deploy"}, Actor: "operator_a", Status: run.StatusRunning, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, src); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "run_src", []run.HostSummary{
		{Host: "web01", OK: 5},
		{Host: "web02", Failures: 1, Worst: "failed"},
		{Host: "web03", Unreachable: 1, Worst: "unreachable"},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	src.Status = run.StatusFailed
	if err := store.Save(ctx, src); err != nil {
		t.Fatalf("Save(finished) error = %v", err)
	}

	relaunch, err := d.RelaunchFailedHosts(ctx, "run_src", "operator_b")
	if err != nil {
		t.Fatalf("RelaunchFailedHosts() error = %v", err)
	}
	// Only the two failed hosts are targeted, and the good one is left alone.
	limit := relaunch.Limit
	if !strings.Contains(limit, "web02") || !strings.Contains(limit, "web03") || strings.Contains(limit, "web01") {
		t.Errorf("relaunch limit = %q, want only web02 and web03", limit)
	}
	if relaunch.RetryOf == nil || *relaunch.RetryOf != "run_src" {
		t.Errorf("relaunch RetryOf = %v, want run_src", relaunch.RetryOf)
	}
	// The spec is inherited, so the source's tags carry onto the relaunch.
	if len(relaunch.Tags) != 1 || relaunch.Tags[0] != "deploy" {
		t.Errorf("relaunch tags = %v, want the source's [deploy]", relaunch.Tags)
	}
	// The relaunch belongs to whoever asked for it. Stamping the source run's actor credited it to
	// the wrong person, so asking what a given operator started missed the runs they started here.
	if relaunch.Actor != "operator_b" {
		t.Errorf("relaunch actor = %q, want the operator who asked for it, operator_b", relaunch.Actor)
	}

	// A run whose every host succeeded has nothing to relaunch.
	green := &run.Run{ID: "run_green", Playbook: "p.yml", Tool: "ansible", Status: run.StatusRunning, CreatedAt: time.Now()}
	_ = store.Save(ctx, green)
	_ = store.SaveHostSummary(ctx, "run_green", []run.HostSummary{{Host: "a", OK: 3}})
	green.Status = run.StatusSucceeded
	_ = store.Save(ctx, green)
	if _, err := d.RelaunchFailedHosts(ctx, "run_green", "operator_b"); !errors.Is(err, ErrNoFailedHosts) {
		t.Errorf("relaunch of an all-green run = %v, want ErrNoFailedHosts", err)
	}

	// A run with no per-host results, such as a bash command, is refused rather than guessed at.
	bash := &run.Run{ID: "run_bash", Tool: "bash", Command: "echo hi", Status: run.StatusFailed, CreatedAt: time.Now()}
	_ = store.Save(ctx, bash)
	if _, err := d.RelaunchFailedHosts(ctx, "run_bash", "operator_b"); !errors.Is(err, ErrNoHostSummary) {
		t.Errorf("relaunch of a run with no host results = %v, want ErrNoHostSummary", err)
	}
}
