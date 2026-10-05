package pgstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// schOldWidth is a next fire time written the way an earlier release wrote it, with a fixed-width
// fraction that FormatTime no longer produces, so the normalization on open rewrites it.
const schOldWidth = "2026-10-04T10:00:00.000000000Z"

// schOldWidthAdvanced is the next fire an earlier release's ClaimDue writes after firing the 10:00
// occurrence of a five minute schedule, in that release's form.
const schOldWidthAdvanced = "2026-10-04T10:05:00.000000000Z"

// schWaitForLockWaiter waits until a backend on the database is blocked on a row lock while
// running an UPDATE of schedules.next_run_at, which is the normalization's write parked behind the
// claim the test is holding open.
func schWaitForLockWaiter(t *testing.T, raw *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := raw.QueryRow(`SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'
AND query LIKE 'UPDATE schedules SET next_run_at=%'`).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the normalization never reached its write")
}

// TestScheduleNormalizeOnOpenRevertsAClaimMadeDuringARollingUpgrade starts a process of this
// release against a database a replica of an earlier release is still firing schedules from, the
// shape of a rolling upgrade of a highly available pair.
//
// Every Open rewrites each stored next fire time into the form this release writes, so the
// compare-and-set in ClaimDue can match it. The rewrite reads the stored stamp and then writes the
// canonical form of what it read with an UPDATE keyed by id alone. When the earlier replica claims
// the due 10:00 occurrence between that read and that write, which the test forces by holding the
// claim's row lock open until the rewrite is parked behind it, the rewrite lands on top of the
// claim and moves the schedule back to 10:00. The occurrence that was just fired is due again, and
// this release's scheduler, which can match the canonical stamp, fires it a second time. A
// compare-and-set on the stamp it read would have left the claim alone.
func TestScheduleNormalizeOnOpenRevertsAClaimMadeDuringARollingUpgrade(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	ctx := context.Background()
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	if err := db.Schedules().Save(ctx, &schedule.Schedule{
		ID: "sch_norm", Name: "every five minutes", Cron: "*/5 * * * *", Timezone: "UTC",
		Playbook: "sync.yml", Enabled: true, CreatedAt: due.Add(-time.Hour), NextRunAt: &due,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw := rawHandle(t, dsn)
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=$1 WHERE id='sch_norm'",
		schOldWidth); err != nil {
		t.Fatalf("store the earlier release's stamp: %v", err)
	}

	// The earlier replica claims the due occurrence and has not committed yet.
	claim, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the claim: %v", err)
	}
	defer func() { _ = claim.Rollback() }()
	res, err := claim.Exec("UPDATE schedules SET next_run_at=$1 WHERE id='sch_norm' AND "+
		"next_run_at=$2", schOldWidthAdvanced, schOldWidth)
	if err != nil {
		t.Fatalf("claim the due occurrence: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("the earlier replica's claim matched %d rows, want 1", n)
	}

	// A process of this release starts and normalizes, the way every Open does.
	starting := rawHandle(t, dsn)
	done := make(chan error, 1)
	go func() { done <- normalizeScheduleTimes(starting) }()
	schWaitForLockWaiter(t, raw)
	if err := claim.Commit(); err != nil {
		t.Fatalf("commit the claim: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normalizeScheduleTimes() error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the normalization did not finish")
	}

	var stored string
	if err := raw.QueryRow("SELECT next_run_at FROM schedules WHERE id='sch_norm'").
		Scan(&stored); err != nil {
		t.Fatalf("read the next fire: %v", err)
	}
	got, err := sqlutil.ParseTime(stored)
	if err != nil {
		t.Fatalf("parse the next fire %q: %v", stored, err)
	}
	if want := due.Add(5 * time.Minute); !got.Equal(want) {
		t.Errorf("after the claim of the 10:00 occurrence the next fire is %s, want %s: the "+
			"normalization wrote the occurrence that was just fired back as due, so it fires again",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
