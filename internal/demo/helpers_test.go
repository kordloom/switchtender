package demo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// errAppendRefused is what the refusing audit store returns, standing in for a chain that cannot be
// written to.
var errAppendRefused = errors.New("chain is read only")

// refusingAudit is an audit.Store whose appends always fail, so the seeder's behavior when the chain
// refuses a write can be observed. The embedded interface is nil: only the methods the seeder calls
// are implemented, and any other call is a test bug rather than a silent pass.
type refusingAudit struct {
	// Store carries the rest of the interface, unimplemented on purpose.
	audit.Store
}

// Append refuses every entry.
func (refusingAudit) Append(context.Context, *audit.Entry) error { return errAppendRefused }

// List returns nothing, since nothing was ever appended.
func (refusingAudit) List(context.Context, int) ([]*audit.Entry, error) { return nil, nil }

// TestEnglishListReadsAsProse pins the warning that tells a demo operator which tools their host
// cannot show. The list is interpolated into a sentence, so a slice printed raw into the middle of
// one is the failure this exists to avoid.
func TestEnglishListReadsAsProse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the list of missing tool names.
		In []string
		// WantResult is the phrase the warning carries.
		WantResult string
	}{
		{In: nil, WantResult: ""},                                        // Test 0: Nothing missing.
		{In: []string{}, WantResult: ""},                                 // Test 1: An empty slice.
		{In: []string{"go"}, WantResult: "go"},                           // Test 2: One name.
		{In: []string{"terraform", "go"}, WantResult: "terraform or go"}, // Test 3: Two names.
		{In: []string{"a", "b", "c"}, WantResult: "a, b, or c"},          // Test 4: Three names.
		{In: []string{"a", "b", "c", "d"}, WantResult: "a, b, c, or d"},  // Test 5: Four names.
		{In: []string{""}, WantResult: ""},                               // Test 6: An empty name.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, englishList(test.In)); diff != "" {
				t.Errorf("englishList(%v) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestFailVarsOnlyTargetsANamedHost pins the switch that makes one host in a seeded run fail. An
// empty host must produce no options at all, because a clean run that silently carried a fail_host
// variable would make every seeded run look broken on the demo.
func TestFailVarsOnlyTargetsANamedHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Host is the host the run should fail on, empty for a clean run.
		Host string
		// WantVars is the extra vars the run carries.
		WantVars map[string]any
	}{
		{Host: "", WantVars: nil},                                         // Test 0: A clean run.
		{Host: "db01", WantVars: map[string]any{"fail_host": "db01"}},     // Test 1: One failing host.
		{Host: "edge01", WantVars: map[string]any{"fail_host": "edge01"}}, // Test 2: A different host.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{}
			opts := failVars(test.Host)
			if test.Host == "" && len(opts) != 0 {
				t.Fatalf("failVars(\"\") returned %d options, want none", len(opts))
			}
			for _, o := range opts {
				o(r)
			}
			if diff := cmp.Diff(test.WantVars, r.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHaveResolvesOnPath pins the check the seeder uses to skip a tool whose binary is absent. A
// check that answered true for a missing binary would leave an exec-not-found failure sitting in the
// demo, which is exactly the broken-looking run the skip exists to prevent.
func TestHaveResolvesOnPath(t *testing.T) {
	// Not parallel: it replaces PATH for the process.
	dir := t.TempDir()
	writeFakeTool(t, dir, "prettysurelyabsenttool")
	t.Setenv("PATH", dir)

	if !have("prettysurelyabsenttool") {
		t.Error("have() = false for a binary that is on PATH")
	}
	if have("definitelynotinstalledanywhere") {
		t.Error("have() = true for a binary that is not on PATH")
	}
	if have("") {
		t.Error("have(\"\") = true, but an empty name resolves to nothing")
	}
}

// TestInfraStepPrefersTerraformAndAlwaysPlans pins the mixed pipeline's provisioning step against
// the two things that matter about it: which tool it picks, and that a Terraform step in a public
// demo is a plan rather than an apply. A step that lost its dry run would have the demo instance
// running real infrastructure changes on every seed.
func TestInfraStepPrefersTerraformAndAlwaysPlans(t *testing.T) {
	// Not parallel: it replaces PATH for the process.
	tests := []struct {
		// Name labels which tools the host has.
		Name string
		// Present are the binaries placed on PATH.
		Present []string
		// WantTool is the tool the provisioning step must use.
		WantTool string
		// WantDryRun is whether the step must be a no-change run.
		WantDryRun bool
	}{
		{Name: "terraform wins", Present: []string{"terraform", "python3"}, WantTool: run.ToolTerraform, WantDryRun: true}, // Test 0.
		{Name: "python is next", Present: []string{"python3"}, WantTool: run.ToolPython},                                   // Test 1.
		{Name: "bash is the floor", Present: nil, WantTool: run.ToolBash},                                                  // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			dir := t.TempDir()
			for _, tool := range test.Present {
				writeFakeTool(t, dir, tool)
			}
			t.Setenv("PATH", dir)

			step := infraStep("/srv/infra/network")
			if step.Name != "provision" {
				t.Errorf("%s: step name = %q, want provision", test.Name, step.Name)
			}
			if step.Tool != test.WantTool {
				t.Errorf("%s: tool = %q, want %q", test.Name, step.Tool, test.WantTool)
			}
			if step.DryRun != test.WantDryRun {
				t.Errorf("%s: dry run = %v, want %v", test.Name, step.DryRun, test.WantDryRun)
			}
			if step.Command == "" {
				t.Errorf("%s: the provisioning step carries no command", test.Name)
			}
			if test.WantTool == run.ToolTerraform && step.Command != "/srv/infra/network" {
				t.Errorf("%s: terraform command = %q, want the working directory", test.Name, step.Command)
			}
		})
	}
}

// writeFakeTool places an executable stub named tool in dir so exec.LookPath resolves it.
func writeFakeTool(t *testing.T, dir, tool string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH stubs are not portable to windows")
	}
	path := filepath.Join(dir, tool)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tool, err)
	}
}

// TestSeedTimeReadsTheSeedClockWhenOneIsSet pins which clock a seeded audit entry is stamped from.
// A creation entry stamped from the wall clock while its run sits hours in the past is the
// contradiction between the chain and the run record that the historical seeding exists to avoid.
func TestSeedTimeReadsTheSeedClockWhenOneIsSet(t *testing.T) {
	t.Parallel()

	// Test 0: with no clock the wall clock is used.
	before := time.Now()
	got := seedTime(Deps{})
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("seedTime() with no clock = %v, want the present", got)
	}

	// Test 1: with a clock the seeded window is used, which is hours in the past.
	clock := NewSeedClock()
	got = seedTime(Deps{Clock: clock})
	if !got.Before(time.Now().Add(-seedRunMargin)) {
		t.Errorf("seedTime() with a clock = %v, want a time inside the seeded window", got)
	}

	// Test 2: successive reads move forward, so entries and runs stay in order.
	next := seedTime(Deps{Clock: clock})
	if !next.After(got) {
		t.Errorf("seedTime() = %v then %v, which does not advance", got, next)
	}
}

// TestSeedOptsStampsProvenanceAndCarriesAReceipt covers what every seeded run is given.
//
// The provenance is what makes the runs list show an origin, an actor, and working label filters
// instead of blanks. The receipt matters more: without it every seeded run answered "no creation
// receipt, so its start cannot be placed on the chain", which is the product's flagship feature
// failing on the one install strangers try first.
func TestSeedOptsStampsProvenanceAndCarriesAReceipt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := audit.NewMemStore()
	deps := Deps{Audit: store}
	labels := map[string]string{"env": "prod", "team": "platform"}

	r := &run.Run{}
	for _, o := range seedOpts(ctx, deps, "schedule", "sch_nightly", "deploy-bot", labels) {
		o(r)
	}

	if r.Source != "schedule" || r.SourceID != "sch_nightly" {
		t.Errorf("source = %q/%q, want schedule/sch_nightly", r.Source, r.SourceID)
	}
	if r.Actor != "deploy-bot" {
		t.Errorf("actor = %q, want deploy-bot", r.Actor)
	}
	if diff := cmp.Diff(labels, r.Labels, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("labels mismatch (-want +got):\n%s", diff)
	}
	if r.AuditReceipt == "" {
		t.Fatal("the run carries no creation receipt, so its start cannot be placed on the chain")
	}

	entries, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the chain holds %d entries, want the one creation entry", len(entries))
	}
	entry := entries[0]
	if entry.Method != "POST" || entry.Path != "/v1/runs" || entry.Actor != "deploy-bot" {
		t.Errorf("creation entry = %+v, want a POST /v1/runs by deploy-bot", entry)
	}
	if diff := cmp.Diff(audit.Receipt(entry), r.AuditReceipt); diff != "" {
		t.Errorf("the receipt does not redeem to the creation entry (-want +got):\n%s", diff)
	}

	// The labels the caller passed are cloned onto the run, so a later edit of the caller's map
	// cannot reach a run that was already submitted.
	labels["env"] = "mutated"
	if r.Labels["env"] != "prod" {
		t.Errorf("the run aliased the caller's label map, env = %q", r.Labels["env"])
	}
}

// TestSeedOptsWithoutAChainStillSeeds pins that the seeder works against a Deps with no audit store
// and against one whose chain refuses writes. Seeding has to finish either way: a demo without a
// chain is still a demo, and a run that could not get a receipt is better than no run at all.
func TestSeedOptsWithoutAChainStillSeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the chain configuration.
		Name string
		// Deps is the seeder dependency set under test.
		Deps Deps
	}{
		{Name: "no chain at all", Deps: Deps{}},                            // Test 0.
		{Name: "a chain that refuses", Deps: Deps{Audit: refusingAudit{}}}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{}
			opts := seedOpts(ctx, test.Deps, "api", "", "admin", map[string]string{"env": "prod"})
			for _, o := range opts {
				o(r)
			}
			if r.Source != "api" || r.Actor != "admin" {
				t.Errorf("%s: provenance was dropped, run = %+v", test.Name, r)
			}
			if r.AuditReceipt != "" {
				t.Errorf("%s: receipt = %q, want none when no entry was written", test.Name, r.AuditReceipt)
			}
		})
	}
}

// TestSeedOptsLetsExtrasWin pins the order the options are applied in. The extras are what carry a
// run's tool, its command, and the approval requirements the governance seed depends on, so an
// extra that landed before the provenance would be overwritten by it and the held run would come out
// as an ordinary Ansible run.
func TestSeedOptsLetsExtrasWin(t *testing.T) {
	t.Parallel()
	r := &run.Run{}
	opts := seedOpts(context.Background(), Deps{}, "schedule", "sch_x", "deploy-bot",
		map[string]string{"env": "prod"},
		run.WithSource("template", "tpl_override"),
		run.WithTool(run.ToolTerraform),
		run.WithCommand("/srv/infra"),
		run.WithRequireApproval(true),
		run.WithRequireDistinctApprover(true),
		run.WithHeldByPolicy("prod terraform destroy"))
	for _, o := range opts {
		o(r)
	}
	if r.Source != "template" || r.SourceID != "tpl_override" {
		t.Errorf("source = %q/%q, want the extra option to win", r.Source, r.SourceID)
	}
	if r.Tool != run.ToolTerraform || r.Command != "/srv/infra" {
		t.Errorf("tool = %q command = %q, want the extras applied", r.Tool, r.Command)
	}
	if !r.RequireDistinctApprover || r.HeldByPolicy == "" {
		t.Errorf("the approval requirements were dropped, run = %+v", r)
	}
}

// TestWaitTerminalReturnsOnStateAndOnCancel pins the two ways the seeder stops waiting for a run.
// Neither may hang: a seed that blocked on a run the engine will never finish would leave the demo
// half populated and the process alive.
func TestWaitTerminalReturnsOnStateAndOnCancel(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()

	// Test 0: a run already in a terminal state returns at once.
	done := &run.Run{ID: run.NewID(), Status: run.StatusSucceeded, CreatedAt: time.Now()}
	if err := store.Save(ctx, done); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	start := time.Now()
	waitTerminal(ctx, store, done.ID)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitTerminal() on a finished run took %v", elapsed)
	}

	// Test 1: a run that never finishes is abandoned when the context is canceled.
	stuck := &run.Run{ID: run.NewID(), Status: run.StatusRunning, CreatedAt: time.Now()}
	if err := store.Save(ctx, stuck); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	waitTerminal(cancelCtx, store, stuck.ID)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitTerminal() ignored the canceled context for %v", elapsed)
	}

	// Test 2: a run that is not in the store at all is abandoned the same way.
	missingCtx, cancelMissing := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelMissing()
	start = time.Now()
	waitTerminal(missingCtx, store, "run_never_created")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitTerminal() on a missing run took %v", elapsed)
	}
}

// TestWaitOutcomeCommittedStopsOnTheRunsOwnEntry pins the wait that keeps a run's outcome entry in
// the same window as its record. Stepping the clock before the outcome lands would stamp the entry
// in the next run's window, which breaks the reconciliation the historical seeding exists to show.
func TestWaitOutcomeCommittedStopsOnTheRunsOwnEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := audit.NewMemStore()
	const runID = "run_abc123"
	if err := store.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "deploy-bot",
		Method: audit.MethodRun, Path: "/v1/runs/" + runID,
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	// Test 0: the outcome is already on the chain, so the wait returns without polling out.
	start := time.Now()
	waitOutcomeCommitted(ctx, store, runID)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitOutcomeCommitted() took %v for an entry already on the chain", elapsed)
	}

	// Test 1: a child that commits no entry of its own is bounded rather than stalling the seed.
	shortCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	start = time.Now()
	waitOutcomeCommitted(shortCtx, store, "run_never_committed")
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Errorf("waitOutcomeCommitted() returned after %v, so it matched an entry it should not have",
			elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("waitOutcomeCommitted() ignored the canceled context for %v", elapsed)
	}
}

// TestSettleStepsTheClockOnlyAfterTheRunHasLanded pins the ordering that keeps each seeded run in
// its own window, and pins that a seeder configured without a clock or a chain still works.
func TestSettleStepsTheClockOnlyAfterTheRunHasLanded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	const runID = "run_settled"
	if err := runs.Save(ctx, &run.Run{ID: runID, Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "deploy-bot",
		Method: audit.MethodRun, Path: "/v1/runs/" + runID,
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	// Test 0: the clock steps by exactly the between-run gap once the run has landed.
	clock := NewSeedClock()
	before := clock.cursor
	settle(ctx, Deps{Runs: runs, Audit: audits, Clock: clock}, runID)
	if moved := clock.cursor.Sub(before); moved != seedRunGap {
		t.Errorf("the clock moved %v, want exactly the between-run gap %v", moved, seedRunGap)
	}

	// Test 1: with no clock and no chain the seed still runs, it simply steps nothing.
	settle(ctx, Deps{Runs: runs}, runID)
}
