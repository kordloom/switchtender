package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/team"
)

// openStore opens a store on a fresh temporary file and closes it when the test ends. Every test
// gets its own file so nothing shares a writer or a WAL with anything else.
func openStore(t *testing.T) *sqlitestore.DB {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openStoreAt opens a store on the named path and closes it when the test ends.
func openStoreAt(t *testing.T, path string) *sqlitestore.DB {
	t.Helper()
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// rawExec runs statements against the database file directly, for building the shapes an older
// release left behind. It uses one connection so PRAGMA settings hold for the whole batch.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw %s: %v", path, err)
	}
	raw.SetMaxOpenConns(1)
	defer func() { _ = raw.Close() }()
	for _, stmt := range stmts {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("raw exec %q: %v", stmt, err)
		}
	}
}

// TestOpenRefusesAPathItCannotUse pins that a bad database path fails at Open rather than at the
// first query. Open is called once at startup, so a failure there stops the process with a
// diagnosable error; a handle that opens lazily and only breaks later would surface as an
// unexplained failure on whichever request happened to touch the database first.
func TestOpenRefusesAPathItCannotUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	junk := filepath.Join(dir, "not-a-database")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not sqlite at all", 64)), 0o600); err != nil {
		t.Fatalf("write junk file: %v", err)
	}

	tests := []struct {
		Name string
		Path string
	}{{ // Test 0: A directory cannot hold a database file.
		Name: "directory", Path: dir,
	}, { // Test 1: A path under a directory that does not exist.
		Name: "missing parent", Path: filepath.Join(dir, "nope", "deeper", "x.db"),
	}, { // Test 2: An existing file whose bytes are not a SQLite database.
		Name: "not a database", Path: junk,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			db, err := sqlitestore.Open(test.Path)
			if err == nil {
				_ = db.Close()
				t.Fatalf("Open(%s) succeeded on %s, want a refusal at startup", test.Path, test.Name)
			}
		})
	}
}

// TestOpenIsIdempotentAcrossRepeatedUpgrades pins that opening the same database over and over
// leaves exactly the same shape and keeps the rows. Every open runs the healer, the schema, and
// fourteen migrations, all of which have to be no-ops on a current database. A migration that is
// not idempotent breaks the second restart, not the first, which is the restart nobody tests.
func TestOpenIsIdempotentAcrossRepeatedUpgrades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "repeat.db")

	first := openStoreAt(t, path)
	if err := first.Runs().Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Now(), OrgID: "org_1",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	shapes := make([][]string, 0, 4)
	for i := 0; i < 4; i++ {
		db, err := sqlitestore.Open(path)
		if err != nil {
			t.Fatalf("Open() pass %d error = %v", i, err)
		}
		got, err := db.Runs().Get(ctx, "run_1")
		if err != nil {
			t.Fatalf("Get() pass %d error = %v", i, err)
		}
		if got.OrgID != "org_1" {
			t.Errorf("pass %d lost the stored org: %+v", i, got)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close() pass %d error = %v", i, err)
		}
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		shapes = append(shapes, tableColumns(t, raw, "runs"))
		if err := raw.Close(); err != nil {
			t.Fatalf("close raw: %v", err)
		}
	}
	for i := 1; i < len(shapes); i++ {
		if diff := cmp.Diff(shapes[0], shapes[i]); diff != "" {
			t.Errorf("open pass %d changed the runs table (-pass0 +pass%d):\n%s", i, i, diff)
		}
	}
}

// TestNormalizeScheduleTimesSurvivesAnUnreadableStamp pins that one broken timestamp cannot stop
// the server from starting. The normalizer rewrites every stored next_run_at so a claim does not
// depend on which release wrote the row, and it walks every schedule to do it. A row it cannot
// parse is left alone by design, because rewriting a value that cannot be read would be a guess.
// Failing the open instead would take the whole install down over one unusable schedule.
func TestNormalizeScheduleTimesSurvivesAnUnreadableStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sched.db")

	db := openStoreAt(t, path)
	next := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, sc := range []*schedule.Schedule{
		{ID: "sc_broken", Name: "broken", Cron: "0 0 * * *", Playbook: "a.yml", NextRunAt: &next},
		{ID: "sc_padded", Name: "padded", Cron: "0 0 * * *", Playbook: "b.yml", NextRunAt: &next},
		{ID: "sc_none", Name: "none", Cron: "0 0 * * *", Playbook: "c.yml"},
	} {
		if err := db.Schedules().Save(ctx, sc); err != nil {
			t.Fatalf("Save(%s) error = %v", sc.ID, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// One row holds a stamp no parser can read, one holds the padded form a past release wrote,
	// and one holds nothing at all. All three shapes reach the normalizer on a real upgrade.
	rawExec(t, path,
		"UPDATE schedules SET next_run_at='whenever' WHERE id='sc_broken'",
		"UPDATE schedules SET next_run_at='2026-07-31T00:00:00.000000000Z' WHERE id='sc_padded'")

	healed, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() with an unreadable stamp = %v, want the open to survive it", err)
	}
	t.Cleanup(func() { _ = healed.Close() })

	// The padded row was rewritten into the canonical form, so it can be claimed again.
	padded, err := healed.Schedules().Get(ctx, "sc_padded")
	if err != nil {
		t.Fatalf("Get(sc_padded) error = %v", err)
	}
	claimed, err := healed.Schedules().ClaimDue(ctx, "sc_padded", *padded.NextRunAt,
		padded.NextRunAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimDue() error = %v", err)
	}
	if !claimed {
		t.Error("a padded row was not normalized, so that schedule can never fire again")
	}

	// The unreadable row is still there, untouched, so an operator can see and repair it rather
	// than finding the store rewrote it into a time nobody chose.
	var stored string
	rawRead(t, path, "SELECT next_run_at FROM schedules WHERE id='sc_broken'", &stored)
	if stored != "whenever" {
		t.Errorf("the unreadable stamp became %q, want it left exactly as stored", stored)
	}

	// A schedule with no next run at all is not a normalization candidate and must stay null.
	none, err := healed.Schedules().Get(ctx, "sc_none")
	if err != nil {
		t.Fatalf("Get(sc_none) error = %v", err)
	}
	if none.NextRunAt != nil {
		t.Errorf("a schedule with no next run gained one: %v", none.NextRunAt)
	}
}

// rawRead scans a single value from the database file directly.
func rawRead(t *testing.T, path, query string, dest ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw %s: %v", path, err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.QueryRow(query).Scan(dest...); err != nil {
		t.Fatalf("raw query %q: %v", query, err)
	}
}

// TestPreChainAuditCheckAllowsEveryStateItMust pins the other side of the pre-chain refusal. The
// check reads the audit table's shape on every open, and it is the first thing Open does, so a
// state it wrongly refuses is an install that will not start. Only one state is genuinely
// unrecoverable: entries written before the chain existed, which carry no sequence.
func TestPreChainAuditCheckAllowsEveryStateItMust(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Setup []string
		Want  bool
	}{{ // Test 0: A database with no audit table at all, which is every brand-new install.
		Name: "no audit table", Want: true,
	}, { // Test 1: A pre-chain table with no rows: nothing to join to a chain, so nothing is lost.
		Name: "pre-chain table but empty",
		Setup: []string{"CREATE TABLE audit_entries (id TEXT PRIMARY KEY, at TEXT NOT NULL, " +
			"actor TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL)"},
		Want: true,
	}, { // Test 2: A pre-chain table holding entries, which cannot be minted into a chain.
		Name: "pre-chain table with entries",
		Setup: []string{"CREATE TABLE audit_entries (id TEXT PRIMARY KEY, at TEXT NOT NULL, " +
			"actor TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL)",
			"INSERT INTO audit_entries VALUES ('a','2026-07-07T00:00:00Z','x','POST','/v1/runs')"},
		Want: false,
	}, { // Test 3: A chain-era table, empty, which is the ordinary upgraded database.
		Name: "chain era table",
		Setup: []string{"CREATE TABLE audit_entries (id TEXT PRIMARY KEY, at TEXT NOT NULL, " +
			"actor TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, " +
			"seq INTEGER NOT NULL DEFAULT 0)"},
		Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "audit.db")
			if len(test.Setup) > 0 {
				rawExec(t, path, test.Setup...)
			}
			db, err := sqlitestore.Open(path)
			if db != nil {
				t.Cleanup(func() { _ = db.Close() })
			}
			if got := err == nil; got != test.Want {
				t.Errorf("Open() on %s opened = %v, want %v (err %v)", test.Name, got, test.Want, err)
			}
		})
	}
}

// TestTwoHandlesOnOneFileSeeEachOther pins the split deployment the run store's own comments rest
// on: a worker process opens the same database file as the server, and the two must observe each
// other's committed writes. It is why the status counts are not cached, and it is the arrangement
// WAL and the single-writer connection exist to support.
func TestTwoHandlesOnOneFileSeeEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	server := openStoreAt(t, path)
	worker, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })

	if err := server.Runs().Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() on the server handle error = %v", err)
	}
	got, err := worker.Runs().Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() on the worker handle error = %v", err)
	}
	if got.Status != run.StatusPending {
		t.Errorf("the worker handle read %q, want the committed pending run", got.Status)
	}

	// The worker claims it, and the server must see the claim, not a cached pending row.
	claimed, err := worker.Runs().Claim(ctx, "worker-1", nil)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != "run_1" {
		t.Fatalf("Claim() took %q, want run_1", claimed.ID)
	}
	back, err := server.Runs().Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() after the worker claim error = %v", err)
	}
	if back.ClaimedBy != "worker-1" {
		t.Errorf("the server handle reads claimed_by = %q, want worker-1", back.ClaimedBy)
	}
	counts, err := server.Runs().RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	if counts[run.StatusPending] != 1 {
		t.Errorf("status counts = %v, want the row the other process is holding", counts)
	}
}

// TestOrphanTeamMembershipBlocksEveryUpgrade demonstrates a first-day database that cannot be
// opened at all.
//
// The rebuild that gives team_members its promised foreign key copies the old rows into a table
// that declares the constraint, with foreign_keys already ON, so a membership row naming a team
// that does not exist aborts the copy and Open returns an error. Nothing starts: no server, no
// audit chain, no runs. The orphan is exactly what the pre-constraint build permitted, since
// AddMember on a table with no foreign key accepted any team id at all, which is the behavior the
// constraint was added to stop.
func TestOrphanTeamMembershipBlocksEveryUpgrade(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "firstday.db")
	db := openStoreAt(t, path)
	if err := db.Teams().Save(ctx, &team.Team{
		ID: "team_real", Name: "ops", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// The table as its first day shaped it, holding one good membership and one whose team was
	// never created, which that build accepted without complaint.
	rawExec(t, path,
		"DROP INDEX IF EXISTS idx_team_members_user",
		"DROP TABLE team_members",
		"CREATE TABLE team_members (team_id TEXT NOT NULL, user_id TEXT NOT NULL, "+
			"PRIMARY KEY (team_id, user_id))",
		"INSERT INTO team_members VALUES ('team_real','user_1')",
		"INSERT INTO team_members VALUES ('team_typo','user_2')")

	healed, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() on a first-day database with one orphan membership = %v; the whole "+
			"install refuses to start over a membership row nobody can see", err)
	}
	t.Cleanup(func() { _ = healed.Close() })

	members, err := healed.Teams().Members(ctx, "team_real")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	if diff := cmp.Diff([]string{"user_1"}, members); diff != "" {
		t.Errorf("memberships after the rebuild (-want +got):\n%s", diff)
	}
}

// TestOrgMembershipsNeverGainTheirPromisedForeignKey demonstrates the gap left beside the team
// membership rebuild.
//
// org_members declares the same ON DELETE CASCADE reference that team_members does, and
// team_members got a rebuild on open because SQLite has no ALTER that adds a constraint. Nothing
// rebuilds org_members, so a database whose copy of that table predates the reference enforces
// nothing forever: a membership can name an organization that does not exist, which is what the
// constraint refuses on every fresh install.
func TestOrgMembershipsNeverGainTheirPromisedForeignKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "orgs.db")
	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	rawExec(t, path,
		"DROP INDEX IF EXISTS idx_org_members_user",
		"DROP TABLE org_members",
		"CREATE TABLE org_members (org_id TEXT NOT NULL, user_id TEXT NOT NULL, "+
			"role TEXT NOT NULL DEFAULT 'member', PRIMARY KEY (org_id, user_id))")

	healed := openStoreAt(t, path)
	if err := healed.Orgs().AddMember(ctx, "org_ghost", "user_1", "admin"); err == nil {
		t.Error("AddMember named an organization that does not exist and was accepted, so an " +
			"upgraded database enforces nothing that a fresh one refuses")
	}
}

// TestFreshOrgMembershipsAreConstrained is the control for the test above: on a database this
// build created, the organization reference is enforced and a deleted organization takes its
// memberships with it. It pins what an upgraded database is supposed to match.
func TestFreshOrgMembershipsAreConstrained(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)

	if err := db.Orgs().AddMember(ctx, "org_ghost", "user_1", "admin"); err == nil {
		t.Error("AddMember to an organization that does not exist succeeded, want a refusal")
	}
}
