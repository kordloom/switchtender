package storetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// testTransitionStatus checks the atomic status move: it changes a row only from the expected
// status, and a second attempt from a status the run has already left changes nothing, so two
// racing approvers cannot both win.
func testTransitionStatus(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := sampleRun("run_t")
	r.Status = run.StatusPendingApproval
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ok, err := store.TransitionStatus(ctx, "run_t", run.StatusPendingApproval, run.StatusPending)
	if err != nil {
		t.Fatalf("TransitionStatus() error = %v", err)
	}
	if !ok {
		t.Fatal("first transition changed nothing, want it to move the run")
	}
	got, err := store.Get(ctx, "run_t")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	ok, err = store.TransitionStatus(ctx, "run_t", run.StatusPendingApproval, run.StatusPending)
	if err != nil {
		t.Fatalf("TransitionStatus() error = %v", err)
	}
	if ok {
		t.Error("second transition changed a row, want a no-op")
	}
	if ok, err := store.TransitionStatus(ctx, "run_missing", run.StatusPending, run.StatusRejected); err != nil || ok {
		t.Errorf("missing run transition = (%v, %v), want (false, nil)", ok, err)
	}
}

// testBackendEdgeParity pins three places the two backends answered differently for the same data.
//
// None of them needed unusual input. INTEGER is 64 bits on SQLite and 32 on PostgreSQL, so a run
// timeout past two billion was stored by one and refused by the other with an encoding error, and
// the same submission answered 202 or 500 depending on the database behind it. step_index is
// nullable and SQLite sorts NULLs first where PostgreSQL sorts them last, so the steps listing, which
// is not gated on kind and therefore lists a split parent's shard children too, came back in a
// different order. And PostgreSQL's LIKE treats a backslash as an escape where SQLite's does not, so
// one search term matched different rows.
func testBackendEdgeParity(t *testing.T, store run.Store) {
	ctx := context.Background()

	// A timeout past what a 32-bit column holds is stored, not refused, and comes back bounded.
	big := sampleRun("run_big")
	big.Timeout = 3_000_000_000
	big.IdempotencyKey = "idem_big"
	if err := store.Save(ctx, big); err != nil {
		t.Fatalf("Save() with an oversized timeout error = %v", err)
	}
	got, err := store.Get(ctx, "run_big")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Timeout != math.MaxInt32 {
		t.Errorf("timeout = %d, want it held at %d so both backends store the same value",
			got.Timeout, math.MaxInt32)
	}

	// Children with a null step index sort last, the same way on both backends.
	parent := sampleRun("run_parent")
	parent.Status = run.StatusRunning
	parent.IdempotencyKey = "idem_parent"
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() parent error = %v", err)
	}
	for i, spec := range []struct {
		ID    string
		Index *int
	}{
		{"run_step_b", intPtr(1)},
		{"run_step_null", nil},
		{"run_step_a", intPtr(0)},
	} {
		child := sampleRun(spec.ID)
		child.ParentID = &parent.ID
		child.StepIndex = spec.Index
		child.IdempotencyKey = fmt.Sprintf("idem_child_%d", i)
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save() child %s error = %v", spec.ID, err)
		}
	}
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	order := make([]string, 0, len(steps))
	for _, st := range steps {
		order = append(order, st.ID)
	}
	want := []string{"run_step_a", "run_step_b", "run_step_null"}
	if diff := cmp.Diff(want, order); diff != "" {
		t.Errorf("steps order mismatch, so the two backends disagree (-want +got):\n%s", diff)
	}

	// Host ordering is byte order on both backends. PostgreSQL's default glibc collation sorts text
	// linguistically, so the same host set came back in a different order, and the summary trim uses
	// the same kind of tiebreaker to choose which rows to delete.
	hostRun := sampleRun("run_hosts")
	hostRun.Status = run.StatusRunning
	hostRun.DryRun = false
	hostRun.IdempotencyKey = "idem_hosts"
	if err := store.Save(ctx, hostRun); err != nil {
		t.Fatalf("Save() host run error = %v", err)
	}
	summaries := make([]run.HostSummary, 0, 4)
	for _, host := range []string{"web1", "Web2", "web-10", "web_3"} {
		summaries = append(summaries, run.HostSummary{Host: host, OK: 1})
	}
	if err := store.SaveHostSummary(ctx, hostRun.ID, summaries); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	health, err := store.FleetHealth(ctx, 50)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	// These four all have zero failures, so they tie on the primary key and the text tiebreaker is
	// the only thing ordering them against each other.
	mine := map[string]bool{"web1": true, "Web2": true, "web-10": true, "web_3": true}
	hosts := make([]string, 0, 4)
	for _, h := range health {
		if mine[h.Host] {
			hosts = append(hosts, h.Host)
		}
	}
	wantHosts := []string{"Web2", "web-10", "web1", "web_3"} // byte order
	if diff := cmp.Diff(wantHosts, hosts); diff != "" {
		t.Errorf("tied hosts are not in byte order, so the two backends order them differently "+
			"(-want +got):\n%s", diff)
	}

	// A backslash in a search term is a literal on both backends, not an escape on one.
	slashed := sampleRun("run_slash")
	slashed.Command = `deploy c:\builds\web`
	slashed.IdempotencyKey = "idem_slash"
	if err := store.Save(ctx, slashed); err != nil {
		t.Fatalf("Save() slashed error = %v", err)
	}
	page, err := store.ListPage(ctx, run.ListFilter{Query: `c:\builds`}, 50, 0)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	var found bool
	for _, r := range page {
		if r.ID == "run_slash" {
			found = true
		}
	}
	if !found {
		t.Errorf("a search term containing a backslash did not match the run holding it, so the "+
			"two backends read the term differently (%d rows returned)", len(page))
	}
}

// testUnrepresentableText checks a run whose text carries a NUL byte or invalid UTF-8 is stored and
// finished the same way on every backend.
//
// The backends disagreed, and the disagreement lost the run. SQLite stores arbitrary bytes, so such a
// run finished normally. PostgreSQL refuses both with SQLSTATE 22021, so the terminal write failed,
// FinalizeRunning reported no change, the run stayed running until the lease sweep interrupted it,
// and the real outcome and exit code were gone. Nothing unusual is needed to reach it: the text comes
// from whatever a tool printed as it failed, from an imported inventory, or from a JSON body
// carrying an escaped NUL. The contract suite never exercised a byte outside ASCII, which is why the
// divergence sat there.
func testUnrepresentableText(t *testing.T, store run.Store) {
	ctx := context.Background()
	// A NUL, a lone continuation byte, and a truncated multi-byte sequence.
	nasty := "boom\x00 \xff\xfe end \xe2\x82"

	r := sampleRun("run_text")
	r.Status = run.StatusRunning
	r.Command = "echo " + nasty
	r.Error = ""
	r.IdempotencyKey = "idem_text"
	r.ExtraVars = map[string]any{"note": nasty, "count": 3}
	r.Labels = map[string]string{"env": nasty}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() with unrepresentable text error = %v", err)
	}

	code := 2
	fin := run.Finalization{
		Status: run.StatusFailed, ExitCode: &code, Error: nasty,
		Warning: nasty, Outputs: map[string]any{"tail": nasty},
		EndedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}
	moved, err := store.FinalizeRunning(ctx, "run_text", fin)
	if err != nil {
		t.Fatalf("FinalizeRunning() with unrepresentable text error = %v", err)
	}
	if !moved {
		t.Fatal("FinalizeRunning() recorded nothing, so the run keeps running and its outcome is lost")
	}

	got, err := store.Get(ctx, "run_text")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != code {
		t.Errorf("exit code = %v, want %d", got.ExitCode, code)
	}
	// What is stored must be readable text, and it must still say something about what happened.
	for _, field := range []struct {
		Name  string
		Value string
	}{
		{"command", got.Command}, {"error", got.Error}, {"warning", got.Warning},
	} {
		if !utf8.ValidString(field.Value) {
			t.Errorf("%s round-tripped as invalid UTF-8: %q", field.Name, field.Value)
		}
		if strings.ContainsRune(field.Value, 0) {
			t.Errorf("%s round-tripped with a NUL byte: %q", field.Name, field.Value)
		}
	}
	if !strings.Contains(got.Error, "boom") || !strings.Contains(got.Error, "end") {
		t.Errorf("error lost the readable part of what the tool said: %q", got.Error)
	}
	if !strings.Contains(got.Command, "echo") {
		t.Errorf("command lost its readable part: %q", got.Command)
	}
}

// testApplyRunningProgress checks the non-terminal write is fenced the same way the terminal one is:
// it records progress for a run still running under the reporting worker, and changes nothing for a
// run that has settled, that another worker now holds, or that does not exist.
//
// The relay used to re-read the run, check it was not terminal, and save the whole row. The sweep
// could settle the run inside that window, and the save then restored the status, the lease, and the
// cleared claim secret from the pre-sweep snapshot. The run came back to life under a lease the
// control node had declared dead, and its later terminal report put a second outcome on the chain
// beside the interrupted one already committed for it.
func testApplyRunningProgress(t *testing.T, store run.Store) {
	ctx := context.Background()
	started := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	progress := run.Progress{
		StartedAt: &started,
		Warning:   "one host was unreachable",
		Outputs:   map[string]any{"stage": "deploy"},
	}

	r := sampleRun("run_prog")
	r.Status = run.StatusRunning
	r.ClaimedBy = "worker-a"
	r.StartedAt = nil
	r.Warning = ""
	r.Outputs = nil
	r.IdempotencyKey = "idem_prog"
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	ok, err := store.ApplyRunningProgress(ctx, "run_prog", "worker-a", progress)
	if err != nil {
		t.Fatalf("ApplyRunningProgress() error = %v", err)
	}
	if !ok {
		t.Fatal("ApplyRunningProgress() changed nothing for a running run it holds")
	}
	got, err := store.Get(ctx, "run_prog")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("started at = %v, want %v", got.StartedAt, started)
	}
	if got.Warning != progress.Warning {
		t.Errorf("warning = %q, want %q", got.Warning, progress.Warning)
	}
	if got.Outputs["stage"] != "deploy" {
		t.Errorf("outputs = %v, want the reported stage", got.Outputs)
	}

	// A repeated report must not move the start time, so a retry cannot rewrite when work began.
	later := started.Add(time.Hour)
	if _, err := store.ApplyRunningProgress(ctx, "run_prog", "worker-a",
		run.Progress{StartedAt: &later}); err != nil {
		t.Fatalf("ApplyRunningProgress(repeat) error = %v", err)
	}
	if got, err = store.Get(ctx, "run_prog"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("a repeated report moved the start time to %v, want %v", got.StartedAt, started)
	}
	// An empty report leaves what is stored alone rather than blanking it.
	if got.Warning != progress.Warning || got.Outputs["stage"] != "deploy" {
		t.Errorf("an empty report cleared stored progress: warning=%q outputs=%v",
			got.Warning, got.Outputs)
	}

	// Another worker's report is refused, so a reclaimed executor cannot write onto the run the
	// worker that replaced it now holds.
	if ok, err := store.ApplyRunningProgress(ctx, "run_prog", "worker-b",
		run.Progress{Warning: "from the wrong worker"}); err != nil {
		t.Fatalf("ApplyRunningProgress(other worker) error = %v", err)
	} else if ok {
		t.Error("a worker that does not hold the run wrote progress onto it")
	}

	// The case that matters: the sweep settles the run, and a report already in flight must not
	// bring it back.
	settled := run.Finalization{Status: run.StatusInterrupted, Error: "executor lease expired",
		EndedAt: time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC)}
	if moved, err := store.FinalizeRunning(ctx, "run_prog", settled); err != nil || !moved {
		t.Fatalf("FinalizeRunning() moved = %v, err = %v", moved, err)
	}
	if ok, err := store.ApplyRunningProgress(ctx, "run_prog", "worker-a",
		run.Progress{Warning: "still going"}); err != nil {
		t.Fatalf("ApplyRunningProgress(settled) error = %v", err)
	} else if ok {
		t.Error("a report resurrected a run the sweep had already settled")
	}
	if got, err = store.Get(ctx, "run_prog"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("status = %q, want it to stay interrupted", got.Status)
	}

	// A run that does not exist changes nothing and is not an error.
	if ok, err := store.ApplyRunningProgress(ctx, "run_missing", "worker-a",
		run.Progress{Warning: "x"}); err != nil {
		t.Fatalf("ApplyRunningProgress(missing) error = %v", err)
	} else if ok {
		t.Error("a missing run reported a change")
	}
}

// testFinalizeRunning checks the terminal write: it moves a running run and records every fact that
// explains how it ended in the same operation, and it changes nothing at all for a run that is not
// running, whether that run is still queued, already terminal, or missing. A store that moved the
// status without the facts would leave a run terminal with no exit code, which no sweep reclaims.
func testFinalizeRunning(t *testing.T, store run.Store) {
	ctx := context.Background()
	ended := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	code := 7
	fin := run.Finalization{
		Status: run.StatusFailed, ExitCode: &code, Error: "the play failed",
		Image: "ghcr.io/example/runner:1.2", CommitSHA: "c0ffee1234567890c0ffee1234567890c0ffee12",
		PullCredentialID: "cred_pull", Outputs: map[string]any{"version": "1.2.3"},
		Warning: "this run recorded no per-host result", EndedAt: ended,
	}

	r := sampleRun("run_fin")
	r.Status = run.StatusRunning
	r.ExitCode = nil
	r.EndedAt = nil
	r.Error = ""
	r.Image = ""
	// Resolved while the run is under way, after the last whole-run save, so the terminal write is
	// their only chance to land.
	r.CommitSHA = ""
	r.PullCredentialID = ""
	r.Outputs = nil
	r.Warning = ""
	r.IdempotencyKey = "idem_fin"
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// A write claiming a lease the run does not carry changes nothing. The relay path is gated on the
	// claim secret at the HTTP layer, but a second in-process dispatcher on a shared database is not:
	// one that lost its heartbeats to a partition, came back after the janitor requeued the run, and
	// found another worker had claimed and started it would otherwise terminalize that worker's live
	// run, because the status fence alone was satisfied by the second worker making it running again.
	stale := fin
	stale.Owner = "worker-that-lost-the-lease"
	if moved, err := store.FinalizeRunning(ctx, "run_fin", stale); err != nil {
		t.Fatalf("FinalizeRunning(stale lease) error = %v", err)
	} else if moved {
		t.Error("a finalize naming a lease the run does not hold was applied")
	}

	ok, err := store.FinalizeRunning(ctx, "run_fin", fin)
	if err != nil {
		t.Fatalf("FinalizeRunning() error = %v", err)
	}
	if !ok {
		t.Fatal("FinalizeRunning() changed nothing for a running run, want it to record the result")
	}
	got, err := store.Get(ctx, "run_fin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != code {
		t.Errorf("exit code = %v, want %d", got.ExitCode, code)
	}
	if got.Error != fin.Error {
		t.Errorf("error = %q, want %q", got.Error, fin.Error)
	}
	if got.Image != fin.Image {
		t.Errorf("image = %q, want %q", got.Image, fin.Image)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("ended_at = %v, want %v", got.EndedAt, ended)
	}
	// The commit the run executed and the credential its image was pulled with are resolved after
	// the last whole-run save. Dropping them leaves the dossier without provenance and narrows the
	// grantable objects the run's own authorization is rebuilt from.
	if got.CommitSHA != fin.CommitSHA {
		t.Errorf("commit_sha = %q, want %q", got.CommitSHA, fin.CommitSHA)
	}
	if got.PullCredentialID != fin.PullCredentialID {
		t.Errorf("pull_credential_id = %q, want %q", got.PullCredentialID, fin.PullCredentialID)
	}
	// Outputs are what the next pipeline step reads as its inputs, and the warning is the run's note
	// about itself. Both are folded as the run finishes, so this write is their only chance to land.
	if diff := cmp.Diff(fin.Outputs, got.Outputs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("outputs mismatch (-want +got):\n%s", diff)
	}
	if got.Warning != fin.Warning {
		t.Errorf("warning = %q, want %q", got.Warning, fin.Warning)
	}

	// A second attempt has nothing to finalize, and it must not rewrite what the first one recorded.
	second := run.Finalization{Status: run.StatusSucceeded, Error: "", Image: "", EndedAt: ended}
	if ok, err := store.FinalizeRunning(ctx, "run_fin", second); err != nil || ok {
		t.Errorf("second FinalizeRunning() = (%v, %v), want (false, nil)", ok, err)
	}
	again, err := store.Get(ctx, "run_fin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Status != run.StatusFailed || again.Error != fin.Error {
		t.Errorf("terminal run changed to (%q, %q), want it left at (failed, %q)",
			again.Status, again.Error, fin.Error)
	}

	// A queued run has not been executed by anybody, so nothing about how it ended can be recorded.
	queued := sampleRun("run_fin_pending")
	queued.Status = run.StatusPending
	queued.ExitCode = nil
	queued.EndedAt = nil
	queued.IdempotencyKey = "idem_fin_pending"
	if err := store.Save(ctx, queued); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if ok, err := store.FinalizeRunning(ctx, "run_fin_pending", fin); err != nil || ok {
		t.Errorf("pending FinalizeRunning() = (%v, %v), want (false, nil)", ok, err)
	}
	stillQueued, err := store.Get(ctx, "run_fin_pending")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stillQueued.Status != run.StatusPending || stillQueued.EndedAt != nil {
		t.Errorf("pending run became (%q, ended %v), want it untouched",
			stillQueued.Status, stillQueued.EndedAt)
	}

	if ok, err := store.FinalizeRunning(ctx, "run_missing", fin); err != nil || ok {
		t.Errorf("missing FinalizeRunning() = (%v, %v), want (false, nil)", ok, err)
	}
}

// testSaveGet verifies a run round trips and that returned values are independent copies.
func testSaveGet(t *testing.T, store run.Store) {
	ctx := context.Background()
	want := sampleRun("run_1")
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}

	got.Playbook = "mutated"
	again, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Playbook != "play.yml" {
		t.Error("mutating the returned run changed stored state")
	}
}

// testGetNotFound verifies a missing run reports run.ErrNotFound across all read methods.
func testGetNotFound(t *testing.T, store run.Store) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
	if _, err := store.Log(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Log() = %v, want ErrNotFound", err)
	}
	if _, err := store.Events(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Events() = %v, want ErrNotFound", err)
	}
	if err := store.AppendLog(ctx, "missing", []byte("x")); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendLog() = %v, want ErrNotFound", err)
	}
	batch := []event.Event{{Type: event.TypePlayStart}}
	if err := store.AppendEvents(ctx, "missing", batch); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendEvents() = %v, want ErrNotFound", err)
	}
}

// testByIdempotencyKey verifies submission dedup at the store: a saved key looks up its run, an
// unused or empty key reports ErrNotFound, re-saving the same run under its key is an ordinary
// update, and a different run claiming a used key is rejected with ErrDuplicateKey without landing,
// the partial unique index that backstops a concurrent retry. An empty key never dedupes.
func testByIdempotencyKey(t *testing.T, store run.Store) {
	ctx := context.Background()

	// An unused key and the empty key are never found.
	if _, err := store.ByIdempotencyKey(ctx, "idem_unused"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("ByIdempotencyKey(unused) = %v, want ErrNotFound", err)
	}
	if _, err := store.ByIdempotencyKey(ctx, ""); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("ByIdempotencyKey(empty) = %v, want ErrNotFound", err)
	}

	// A saved run is found by its key.
	first := &run.Run{
		ID: "run_a", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem_1",
	}
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	got, err := store.ByIdempotencyKey(ctx, "idem_1")
	if err != nil {
		t.Fatalf("ByIdempotencyKey() error = %v", err)
	}
	if got.ID != "run_a" {
		t.Errorf("ByIdempotencyKey() id = %q, want run_a", got.ID)
	}

	// Re-saving the same run under the same key is an ordinary update, not a conflict.
	first.Status = run.StatusRunning
	if err := store.Save(ctx, first); err != nil {
		t.Errorf("re-Save(first) error = %v, want nil", err)
	}

	// A different run claiming the used key is rejected, the backstop for a concurrent retry.
	second := &run.Run{
		ID: "run_b", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem_1",
	}
	if err := store.Save(ctx, second); !errors.Is(err, run.ErrDuplicateKey) {
		t.Errorf("Save(second) = %v, want ErrDuplicateKey", err)
	}
	// The loser never landed: the key still resolves to the original winner, and run_b is absent.
	if got, err := store.ByIdempotencyKey(ctx, "idem_1"); err != nil || got.ID != "run_a" {
		t.Errorf("ByIdempotencyKey() after conflict = (%v, %v), want run_a", got, err)
	}
	if _, err := store.Get(ctx, "run_b"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(run_b) = %v, want ErrNotFound, the losing run must not persist", err)
	}

	// An empty key never dedupes: keyless runs coexist freely.
	for _, id := range []string{"run_c", "run_d"} {
		keyless := &run.Run{ID: id, Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now()}
		if err := store.Save(ctx, keyless); err != nil {
			t.Errorf("Save(%s) keyless error = %v, want nil", id, err)
		}
	}
}

// testSaveUpdate verifies that saving an existing id replaces the stored run.
func testSaveUpdate(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_1", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now()}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	code := 2
	r.Status = run.StatusFailed
	r.ExitCode = &code
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("after update got status=%q exit=%v, want failed exit=2", got.Status, got.ExitCode)
	}
}

// testList verifies runs come back newest first.
func testList(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "a", Status: run.StatusSucceeded, CreatedAt: base},
		{ID: "b", Status: run.StatusSucceeded, CreatedAt: base.Add(time.Second)},
		{ID: "c", Status: run.StatusSucceeded, CreatedAt: base.Add(2 * time.Second)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := make([]string, len(runs))
	for i, r := range runs {
		gotIDs[i] = r.ID
	}
	if diff := cmp.Diff([]string{"c", "b", "a"}, gotIDs); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// testListPage verifies paging returns newest first, honors limit and offset, and that status
// counts tally every top-level run.
func testListPage(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := []*run.Run{
		{ID: "a", Status: run.StatusSucceeded, Playbook: "deploy.yml", CreatedAt: base},
		{ID: "b", Status: run.StatusFailed, Tool: run.ToolBash, Command: "echo hi", CreatedAt: base.Add(time.Second)},
		{ID: "c", Status: run.StatusRunning, Playbook: "migrate.yml", CreatedAt: base.Add(2 * time.Second)},
		{ID: "d", Status: run.StatusSucceeded, Playbook: "deploy.yml", CreatedAt: base.Add(3 * time.Second)},
	}
	for _, r := range seed {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	ids := func(runs []*run.Run) []string {
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ID
		}
		return out
	}

	first, err := store.ListPage(ctx, run.ListFilter{}, 2, 0)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	if diff := cmp.Diff([]string{"d", "c"}, ids(first)); diff != "" {
		t.Errorf("ListPage(2,0) mismatch (-want +got):\n%s", diff)
	}
	next, err := store.ListPage(ctx, run.ListFilter{}, 2, 2)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	if diff := cmp.Diff([]string{"b", "a"}, ids(next)); diff != "" {
		t.Errorf("ListPage(2,2) mismatch (-want +got):\n%s", diff)
	}
	if past, _ := store.ListPage(ctx, run.ListFilter{}, 2, 10); len(past) != 0 {
		t.Errorf("ListPage past end = %v, want empty", ids(past))
	}
	if all, _ := store.ListPage(ctx, run.ListFilter{}, 0, 0); len(all) != 4 {
		t.Errorf("ListPage(0,0) len = %d, want 4", len(all))
	}

	// A non-empty query filters case-insensitively across the runs-view fields, newest first, and
	// composes with paging.
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "deploy"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage search deploy = %v, want [d a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "BASH"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage search BASH = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "failed"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage search failed = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "nomatch"}, 0, 0); len(hit) != 0 {
		t.Errorf("ListPage search nomatch = %v, want empty", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "deploy"}, 1, 0); len(hit) != 1 || hit[0].ID != "d" {
		t.Errorf("ListPage search with page = %v, want [d]", ids(hit))
	}

	// The status and tool filters are exact, unlike the fuzzy query, and the ansible tool matches
	// runs stored with an empty tool, its historical form.
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "failed"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage status failed = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "succeeded"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage status succeeded = %v, want [d a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Tool: "bash"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage tool bash = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Tool: "ansible"}, 0, 0); cmp.Diff([]string{"d", "c", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage tool ansible = %v, want [d c a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "succeeded", Query: "deploy"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage status plus query = %v, want [d a]", ids(hit))
	}

	// A created-at window keeps only runs inside it: half-open, after inclusive, before exclusive.
	if got, _ := store.ListPage(ctx, run.ListFilter{
		After:  base.Add(1 * time.Second),
		Before: base.Add(3 * time.Second),
	}, 0, 0); len(got) != 2 {
		t.Errorf("date window = %d runs, want 2", len(got))
	}

	// OldestFirst flips the default ordering.
	if all, _ := store.ListPage(ctx, run.ListFilter{OldestFirst: true}, 0, 0); cmp.Diff([]string{"a", "b", "c", "d"}, ids(all)) != "" {
		t.Errorf("ListPage oldest first = %v, want [a b c d]", ids(all))
	}

	// A limit alongside OldestFirst returns the earliest runs and no more. The change register
	// bounds itself this way, and a store that answered with the whole window would hand it the
	// unbounded read the bound exists to prevent, on that store alone.
	if got, _ := store.ListPage(ctx, run.ListFilter{OldestFirst: true}, 2, 0); cmp.Diff([]string{"a", "b"}, ids(got)) != "" {
		t.Errorf("ListPage oldest first, limit 2 = %v, want [a b]", ids(got))
	}
	if got, _ := store.ListPage(ctx, run.ListFilter{
		After: base.Add(1 * time.Second), Before: base.Add(4 * time.Second), OldestFirst: true,
	}, 2, 0); cmp.Diff([]string{"b", "c"}, ids(got)) != "" {
		t.Errorf("ListPage window, oldest first, limit 2 = %v, want [b c]", ids(got))
	}

	counts, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	want := map[run.Status]int{run.StatusSucceeded: 2, run.StatusFailed: 1, run.StatusRunning: 1}
	if diff := cmp.Diff(want, counts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("RunStatusCounts() mismatch (-want +got):\n%s", diff)
	}

	// Provenance and label filters compose with the rest. Summaries attach while the run is
	// still live, since the terminal fence rejects summary writes after a run finishes.
	live := &run.Run{
		ID: "e", Playbook: "tag.yml", Status: run.StatusRunning, CreatedAt: base.Add(4 * time.Second),
		Source: "schedule", SourceID: "sch_9", Actor: "night-cron",
		AuditReceipt: "41:9f2caa",
		HeldByPolicy: "prod terraform destroy",
		Labels:       map[string]string{"env": "prod"},
	}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "e", []run.HostSummary{{Host: "web09", Worst: "ok", RanAt: base}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	live.Status = run.StatusSucceeded
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Source: "schedule"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("source filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Actor: "night-cron"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("actor filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{SourceID: "sch_9"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("source id filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{LabelKey: "env", LabelValue: "prod"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("label filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Host: "web09"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("host filter = %v, want [e]", ids(hit))
	}

	// The audit receipt ties a run to the chain entry that recorded the request creating it. It is
	// the only link between the two, since that entry names the request path rather than the run.
	if got, err := store.Get(ctx, "e"); err != nil {
		t.Fatalf("Get(e) error = %v", err)
	} else if got.AuditReceipt != "41:9f2caa" {
		t.Errorf("audit receipt = %q, want it to survive the round trip", got.AuditReceipt)
	} else if got.HeldByPolicy != "prod terraform destroy" {
		// The rule that held a run is evidence, and a policy can be renamed or deleted before
		// anyone reads it, so the name has to survive on the run itself.
		t.Errorf("held by policy = %q, want it to survive the round trip", got.HeldByPolicy)
	}

	// The status tally follows a transition immediately: a store may memoize it, but a stale
	// tally after a write is a wrong number on the runs page. At this point a, d, and e have
	// succeeded, b has failed, and c still runs.
	before, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	// Mutating the returned map must not corrupt what the store serves next.
	before[run.StatusSucceeded] = 999
	if ok, err := store.TransitionStatus(ctx, "c", run.StatusRunning, run.StatusFailed); err != nil || !ok {
		t.Fatalf("TransitionStatus(c) = %v, %v, want true, nil", ok, err)
	}
	after, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() after transition error = %v", err)
	}
	wantAfter := map[run.Status]int{run.StatusSucceeded: 3, run.StatusFailed: 2}
	if diff := cmp.Diff(wantAfter, after, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("RunStatusCounts() after transition mismatch (-want +got):\n%s", diff)
	}
}

// testPaginationAtVolume proves offset paging over the run list and cursor paging over a run's events
// stay correct across many pages, not just the handful other tests use. Every run comes back exactly
// once in newest-first order with no gap or duplicate at a page boundary, a deep offset lands on the
// right slice, and a run's events stream back once each in store-sequence order.
func testPaginationAtVolume(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 3000
	// Zero-padded ids so lexical order matches numeric order, and increasing created times so newest
	// first is a strict, checkable ordering.
	for i := range n {
		r := &run.Run{
			ID: fmt.Sprintf("r%05d", i), Status: run.StatusSucceeded, Playbook: "p.yml",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%d) error = %v", i, err)
		}
	}

	// Page the whole list newest first: every id once, strictly descending, no boundary gap or repeat.
	const page = 100
	seen := make(map[string]bool, n)
	prevID := ""
	got := 0
	for offset := 0; ; offset += page {
		batch, err := store.ListPage(ctx, run.ListFilter{}, page, offset)
		if err != nil {
			t.Fatalf("ListPage(%d,%d) error = %v", page, offset, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			if seen[r.ID] {
				t.Fatalf("run %s came back twice across pages", r.ID)
			}
			seen[r.ID] = true
			if prevID != "" && r.ID >= prevID {
				t.Fatalf("run order broke at %s after %s: not strictly newest first", r.ID, prevID)
			}
			prevID = r.ID
			got++
		}
	}
	if got != n {
		t.Fatalf("paged %d runs, want %d; a page boundary dropped or duplicated runs", got, n)
	}

	// A deep offset lands on the oldest slice, not off the end or on the wrong rows.
	deep, err := store.ListPage(ctx, run.ListFilter{}, 10, n-5)
	if err != nil {
		t.Fatalf("deep ListPage error = %v", err)
	}
	if len(deep) != 5 || deep[0].ID != "r00004" {
		t.Fatalf("deep page has %d rows starting %q, want 5 starting r00004", len(deep),
			func() string {
				if len(deep) == 0 {
					return ""
				}
				return deep[0].ID
			}())
	}

	// Cursor paging over one run's events returns every event once, in sequence order. A still-running
	// run, since a terminal run fences further event writes.
	if err := store.Save(ctx, &run.Run{
		ID: "revents", Status: run.StatusRunning, Playbook: "p.yml", CreatedAt: base,
	}); err != nil {
		t.Fatalf("Save(revents) error = %v", err)
	}
	events := make([]event.Event, n)
	for i := range events {
		events[i] = event.Event{Type: event.TypeRunnerOK, Host: "h", Task: fmt.Sprintf("t%05d", i)}
	}
	if err := store.AppendEvents(ctx, "revents", events); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	var after int64
	streamed := 0
	for {
		batch, err := store.EventsAfter(ctx, "revents", after, page)
		if err != nil {
			t.Fatalf("EventsAfter(%d) error = %v", after, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if e.Seq <= after {
				t.Fatalf("event seq %d is not above the cursor %d, so paging can loop or skip", e.Seq, after)
			}
		}
		after = batch[len(batch)-1].Seq
		streamed += len(batch)
		if len(batch) < page {
			break
		}
	}
	if streamed != n {
		t.Fatalf("cursor-paged %d events, want %d", streamed, n)
	}
}

// testShards verifies that shard runs are excluded from List and returned by Shards in order.
func testShards(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_parent"
	if err := store.Save(ctx,
		&run.Run{ID: parentID, Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx, count := i, 2
		child := &run.Run{
			ID: fmt.Sprintf("run_child_%d", i), Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count, Limit: "host" + fmt.Sprint(i),
		}
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	parentSeen := false
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned shard run %s", r.ID)
		}
		if r.ID == parentID {
			parentSeen = true
		}
	}
	if !parentSeen {
		t.Error("List did not return the parent run")
	}

	shards, err := store.Shards(ctx, parentID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("Shards len = %d, want 2", len(shards))
	}
	if shards[0].ShardIndex == nil || *shards[0].ShardIndex != 0 {
		t.Errorf("first shard index = %v, want 0", shards[0].ShardIndex)
	}
	if shards[0].Limit != "host0" {
		t.Errorf("shard limit = %q, want host0", shards[0].Limit)
	}
}

// testSteps verifies pipeline step runs are ordered by step index and excluded from List.
func testSteps(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_pipeline"
	if err := store.Save(ctx, &run.Run{
		ID: parentID, Kind: run.KindPipeline, Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx := i
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_step_%d", i), Playbook: fmt.Sprintf("step%d.yml", i),
			Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, StepIndex: &idx, StepName: fmt.Sprintf("step-%d", i),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	steps, err := store.Steps(ctx, parentID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if steps[0].StepName != "step-0" || steps[0].StepIndex == nil || *steps[0].StepIndex != 0 {
		t.Errorf("first step = %+v, want name step-0 index 0", steps[0])
	}

	parent, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.Kind != run.KindPipeline {
		t.Errorf("parent kind = %q, want %q", parent.Kind, run.KindPipeline)
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned step run %s", r.ID)
		}
	}
}

// testNonTerminal verifies only pending and running runs are returned, including shards.
func testNonTerminal(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "p"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "pending", Status: run.StatusPending, CreatedAt: time.Now()},
		{ID: "running", Status: run.StatusRunning, CreatedAt: time.Now()},
		{ID: "done", Status: run.StatusSucceeded, CreatedAt: time.Now()},
		{ID: "gone", Status: run.StatusInterrupted, CreatedAt: time.Now()},
		{
			ID: "shard", Status: run.StatusRunning, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.NonTerminal(ctx)
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	seen := make(map[string]bool, len(got))
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen["pending"] || !seen["running"] || !seen["shard"] {
		t.Errorf("NonTerminal missing active runs, got %v", seen)
	}
	if seen["done"] || seen["gone"] {
		t.Error("NonTerminal returned a terminal run")
	}
}

// testProvenance verifies the provenance and label fields round trip through Save and Get.
func testProvenance(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_prov", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Source:    "template", SourceID: "tpl_9", Actor: "douglas",
		RerunOf: "run_prev", Labels: map[string]string{"env": "prod", "ticket": "OPS-1"},
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_prov")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Source != "template" || got.SourceID != "tpl_9" || got.Actor != "douglas" ||
		got.RerunOf != "run_prev" {
		t.Errorf("provenance = %q %q %q %q, want template tpl_9 douglas run_prev",
			got.Source, got.SourceID, got.Actor, got.RerunOf)
	}
	if diff := cmp.Diff(saved.Labels, got.Labels); diff != "" {
		t.Errorf("labels mismatch (-want +got):\n%s", diff)
	}
}

// testWarning verifies a run's warning round trips through Save and Get without touching its status.
// A run whose event capture failed finishes succeeded but has nothing to show, so the warning is the
// only thing that explains the empty matrix and it has to survive the store.
func testWarning(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_warned", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Warning:   "event capture unavailable: no space left on device",
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_warned")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(saved.Warning, got.Warning); diff != "" {
		t.Errorf("warning mismatch (-want +got):\n%s", diff)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded left alone by the warning", got.Status)
	}
}

// testPipelineSteps verifies a pipeline's step graph round trips through Save and Get, including the
// dependencies between steps. A pipeline held for approval is executed from the stored graph, so
// losing it would mean an approved workflow could no longer run, and an ordinary run must store no
// steps at all rather than an empty list.
func testPipelineSteps(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_pipe", Playbook: "release", Kind: run.KindPipeline,
		Status: run.StatusPendingApproval, CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Steps: []run.PipelineStep{
			{Name: "plan", Tool: "terraform", Command: "terraform plan"},
			{Name: "apply", Tool: "terraform", Command: "terraform apply", DependsOn: []string{"plan"},
				Retries: 2, ContinueOnFailure: true},
		},
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_pipe")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(saved.Steps, got.Steps); diff != "" {
		t.Errorf("steps mismatch (-want +got):\n%s", diff)
	}

	plain := &run.Run{
		ID: "run_plain", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, plain); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gotPlain, err := store.Get(ctx, "run_plain")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(gotPlain.Steps) != 0 {
		t.Errorf("plain run steps = %v, want none", gotPlain.Steps)
	}
}

// testStoreAgreementOnEdges pins the paging, label, and ordering answers the three implementations
// used to disagree about.
//
// memStore is test-only, but every dispatch test runs against it, so dispatch was verified against a
// store that behaved differently from both production ones. None of these disagreements produced an
// error; each returned a plausible wrong answer.
func testStoreAgreementOnEdges(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		labels := map[string]string{"app.tier": "web", "env": "prod"}
		if i == 4 {
			labels = nil
		}
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_edge_%d", i), Playbook: "site.yml", Status: run.StatusSucceeded,
			CreatedAt: base.Add(time.Duration(i) * time.Minute), Labels: labels,
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	// An offset with no limit is not a page. Both SQL stores emit OFFSET only alongside LIMIT.
	all, err := store.ListPage(ctx, run.ListFilter{}, 0, 3)
	if err != nil {
		t.Fatalf("ListPage(limit 0) error = %v", err)
	}
	if len(all) < 5 {
		t.Errorf("ListPage(limit=0, offset=3) returned %d rows; an offset without a limit must not "+
			"skip, because that is what both SQL stores do", len(all))
	}

	// A dotted label key is an ordinary key, not a path.
	dotted, err := store.ListPage(ctx, run.ListFilter{LabelKey: "app.tier", LabelValue: "web"}, 50, 0)
	if err != nil {
		t.Fatalf("ListPage(dotted label) error = %v", err)
	}
	if len(dotted) == 0 {
		t.Error("a label key containing a dot matched nothing; keys like app.tier and k8s.io/name " +
			"are ordinary and the list comes back empty with no error")
	}

	// An empty value matches a label that is present and empty, never a label that is absent.
	absent, err := store.ListPage(ctx, run.ListFilter{LabelKey: "env", LabelValue: ""}, 50, 0)
	if err != nil {
		t.Fatalf("ListPage(empty label value) error = %v", err)
	}
	for _, rn := range absent {
		if _, ok := rn.Labels["env"]; !ok {
			t.Errorf("run %s carries no env label but matched a filter for one", rn.ID)
		}
	}

	// Children with no shard index sort last. Indexed shards are the ordered fan-out; an unindexed
	// child is the exception. All three stores say so explicitly, because SQLite orders nulls first
	// and Postgres orders them last, so leaving it to the default made them disagree.
	parent := "run_edge_parent"
	idx := 0
	if err := store.Save(ctx, &run.Run{
		ID: parent, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
		CreatedAt: base,
	}); err != nil {
		t.Fatalf("Save() parent error = %v", err)
	}
	count := 1
	for _, c := range []*run.Run{
		{ID: "run_edge_c1", ParentID: &parent, ShardIndex: &idx, ShardCount: &count},
		{ID: "run_edge_cnil", ParentID: &parent},
	} {
		c.Playbook, c.Status, c.CreatedAt = "site.yml", run.StatusPending, base
		if err := store.Save(ctx, c); err != nil {
			t.Fatalf("Save(%s) error = %v", c.ID, err)
		}
	}
	shards, err := store.Shards(ctx, parent)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 2 && shards[len(shards)-1].ID != "run_edge_cnil" {
		t.Errorf("Shards() = [%s %s]; a child with no shard index sorts last on every backend",
			shards[0].ID, shards[1].ID)
	}
}

// testTransitionStatusAndClaim verifies a run moves status and gains an owner in one step.
//
// Two separate writes leave a window either way. Transition first and a parent is running with no
// lease, which is exactly what the abandoned-parent sweep settles, so a janitor tick landing there
// cancels a run an approver just released. Lease first and a process that dies before the
// transition leaves the run held with an owner, which CancelPending refuses to touch, so it can
// never be canceled either. Neither state exists if the store does both at once.
func testTransitionStatusAndClaim(t *testing.T, store run.Store) {
	ctx := context.Background()
	held := &run.Run{
		ID: "run_atomic", Playbook: "site.yml", Kind: run.KindSplit,
		Status: run.StatusPendingApproval, CreatedAt: time.Now().Add(-time.Hour),
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	ok, err := store.TransitionStatusAndClaim(ctx, held.ID,
		run.StatusPendingApproval, run.StatusRunning, "coordinator-a")
	if err != nil {
		t.Fatalf("TransitionStatusAndClaim() error = %v", err)
	}
	if !ok {
		t.Fatal("the transition reported no change on a run in the expected status")
	}
	got, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.ClaimedBy != "coordinator-a" || got.ClaimedAt == nil {
		t.Errorf("run is %q with no owner recorded, which is the state the sweep settles",
			got.Status)
	}
	if got.StartedAt == nil {
		t.Error("no start time recorded, so nothing can tell how long it has been running")
	}

	// A sweep landing immediately afterwards must leave it alone, because it is owned.
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if after, err := store.Get(ctx, held.ID); err != nil {
		t.Fatalf("Get() after sweep error = %v", err)
	} else if after.Status != run.StatusRunning {
		t.Errorf("status after sweep = %q, want running: a freshly leased run is being coordinated",
			after.Status)
	}

	// Only one caller wins, so two approvals cannot both release the same run.
	second, err := store.TransitionStatusAndClaim(ctx, held.ID,
		run.StatusPendingApproval, run.StatusRunning, "coordinator-b")
	if err != nil {
		t.Fatalf("second TransitionStatusAndClaim() error = %v", err)
	}
	if second {
		t.Error("a second transition from a status the run no longer holds reported a change")
	}
	if missing, err := store.TransitionStatusAndClaim(ctx, "run_nope",
		run.StatusPendingApproval, run.StatusRunning, "x"); err != nil || missing {
		t.Errorf("transition on a missing run = (%v, %v), want (false, nil)", missing, err)
	}

	// A requested cancel refuses the claim. Cancel is a flag rather than a status, so a run canceled
	// after approval and before a coordinator picked it up still reads as holding the status the
	// caller swaps from. Without this the swap succeeds and the run executes on real hosts.
	canceled := &run.Run{
		ID: "run_atomic_canceled", Playbook: "site.yml", Kind: run.KindPipeline,
		Status: run.StatusPendingApproval, CreatedAt: time.Now().Add(-time.Hour),
		CancelRequested: true,
	}
	if err := store.Save(ctx, canceled); err != nil {
		t.Fatalf("Save() canceled error = %v", err)
	}
	started, err := store.TransitionStatusAndClaim(ctx, canceled.ID,
		run.StatusPendingApproval, run.StatusRunning, "coordinator-c")
	if err != nil {
		t.Fatalf("TransitionStatusAndClaim() canceled error = %v", err)
	}
	if started {
		t.Error("a run whose cancel was already requested was claimed and started")
	}
	if after, err := store.Get(ctx, canceled.ID); err != nil {
		t.Fatalf("Get() canceled error = %v", err)
	} else if after.Status == run.StatusRunning || after.ClaimedBy != "" {
		t.Errorf("canceled run is %q claimed by %q, want it left unclaimed",
			after.Status, after.ClaimedBy)
	}
}

// testRunTimings verifies the narrow read the metrics endpoint uses returns the newest top-level
// runs with their timings, and excludes children.
//
// It reads seven columns instead of whole rows because a scrape happens every few seconds and a run
// row carries its extra vars, steps, labels, and notification targets. The contract is that it agrees
// with the full read about which runs exist and when they ran, so a cheaper query does not become a
// different answer.
func testRunTimings(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	started := base.Add(time.Minute)
	ended := base.Add(3 * time.Minute)
	parentID := "run_t_parent"

	for _, r := range []*run.Run{
		{ID: "run_t_old", Playbook: "p", Status: run.StatusSucceeded, Queue: "prod",
			CreatedAt: base, StartedAt: &started, EndedAt: &ended, ClaimedBy: "worker-a"},
		{ID: "run_t_new", Playbook: "p", Status: run.StatusRunning, Queue: "dmz",
			CreatedAt: base.Add(time.Hour), ClaimedBy: "worker-b"},
		{ID: parentID, Playbook: "p", Kind: run.KindSplit, Status: run.StatusRunning,
			CreatedAt: base.Add(2 * time.Hour)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	// A child must not appear: the histograms describe runs somebody asked for, and a shard is part
	// of one that is already counted.
	idx, count := 0, 1
	if err := store.Save(ctx, &run.Run{
		ID: "run_t_child", Playbook: "p", Status: run.StatusRunning, CreatedAt: base.Add(3 * time.Hour),
		ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
	}); err != nil {
		t.Fatalf("Save(child) error = %v", err)
	}

	got, err := store.RunTimings(ctx, 10)
	if err != nil {
		t.Fatalf("RunTimings() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("timings = %d, want the three top-level runs", len(got))
	}
	// Newest first, matching the list order the metrics window assumes.
	if !got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Errorf("timings are not newest first: %v then %v", got[0].CreatedAt, got[1].CreatedAt)
	}
	var oldest run.RunTiming
	for _, tm := range got {
		if tm.Queue == "prod" {
			oldest = tm
		}
	}
	if oldest.Status != run.StatusSucceeded || oldest.ClaimedBy != "worker-a" {
		t.Errorf("timing = %+v, want the succeeded run held by worker-a", oldest)
	}
	if oldest.StartedAt == nil || oldest.EndedAt == nil {
		t.Fatalf("timing carries no start or end: %+v", oldest)
	}
	if !oldest.StartedAt.Equal(started) || !oldest.EndedAt.Equal(ended) {
		t.Errorf("timings = %v to %v, want %v to %v", oldest.StartedAt, oldest.EndedAt, started, ended)
	}

	// The limit bounds the window, which is what keeps a scrape cheap as history grows.
	short, err := store.RunTimings(ctx, 1)
	if err != nil {
		t.Fatalf("RunTimings(1) error = %v", err)
	}
	if len(short) != 1 {
		t.Errorf("limited timings = %d, want 1", len(short))
	}
}
