package sqlitestore

import (
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestEveryLaterColumnIsHealable enforces the invariant the schema-derived healer rests on: a column
// added after its table first shipped must be one ALTER TABLE can add, which means it carries a
// DEFAULT and is not a primary key member. The healer skips non-addable columns on the reasoning that
// they are original-era, and that reasoning is already violated once in this schema: audit_anchors
// gained shape as NOT NULL with no default, and only its hand-written ALTER, which does carry a
// default, keeps pre-shape databases upgradable. Nothing but this test stops the next such column,
// and the next one may not come with a hand migration, because the healer's existence is precisely
// the reason an author would no longer think to write one.
//
// The allowlist is every non-addable column the schema holds today. Growing it is a deliberate act:
// either give the new column a DEFAULT so the healer covers it, or add a hand migration beside it and
// its name here, with a comment saying which upgrade path covers it.
func TestEveryLaterColumnIsHealable(t *testing.T) {
	t.Parallel()
	// Non-addable columns that are allowed to exist, as table.column. Primary keys and the original
	// NOT NULL columns of each table are here by construction; shape is the one late arrival, covered
	// by its own ALTER in migrateAnchors.
	allowed := map[string]string{
		"audit_anchors.shape": "added 2026-07-31 without a default; pre-shape databases are upgraded " +
			"by the hand ALTER in the sqlite migrations and the pg schema blob",
	}

	var violations []string
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
		for i, col := range cols {
			if col.Addable() {
				continue
			}
			// The first column of every table is its id or key, original by construction. Anything
			// else non-addable must be explicitly accounted for.
			if i == 0 {
				continue
			}
			key := fmt.Sprintf("%s.%s", table, col.Name)
			if _, ok := allowed[key]; ok {
				continue
			}
			// Original-era NOT NULL columns without defaults exist in several tables; they are fine
			// because a created table always has them. The line between "original" and "late" cannot
			// be read from the schema alone, so the allowlist grows by review: a failure here means a
			// human decides which case this is, which is the entire point.
			violations = append(violations, key+" ("+col.Clause+")")
		}
	}
	sort.Strings(violations)

	// The full current set, so a change to it is visible in review rather than absorbed. If this diff
	// fires because you added a column: give it a DEFAULT, or write a migration and allow it above.
	wantNonAddable := []string{
		"audit_anchors.at (TEXT NOT NULL)",
		"audit_anchors.link (TEXT NOT NULL)",
		"audit_anchors.seq (INTEGER NOT NULL)",
		"audit_anchors.type (TEXT NOT NULL)",
		"audit_entries.at (TEXT NOT NULL)",
		"audit_entries.method (TEXT NOT NULL)",
		"audit_entries.path (TEXT NOT NULL)",
		"credential_types.name (TEXT NOT NULL)",
		"credentials.created_at (TEXT NOT NULL)",
		"credentials.kind (TEXT NOT NULL)",
		"credentials.secret (TEXT NOT NULL)",
		"grants.access (TEXT NOT NULL)",
		"grants.created_at (TEXT NOT NULL)",
		"grants.object (TEXT NOT NULL)",
		"grants.subject (TEXT NOT NULL)",
		"host_facts.facts (TEXT NOT NULL)",
		"host_facts.gathered_at (TEXT NOT NULL)",
		"host_facts.run_id (TEXT NOT NULL)",
		"inventories.content (TEXT NOT NULL)",
		"inventories.created_at (TEXT NOT NULL)",
		"inventory_sources.created_at (TEXT NOT NULL)",
		"inventory_sources.source (TEXT NOT NULL)",
		"org_members.user_id (TEXT NOT NULL)",
		"orgs.created_at (TEXT NOT NULL)",
		"policies.created_at (TEXT NOT NULL)",
		"projects.created_at (TEXT NOT NULL)",
		"projects.repo_url (TEXT NOT NULL)",
		"run_events.data (TEXT NOT NULL)",
		"run_events.run_id (TEXT NOT NULL)",
		"run_host_summary.changed (INTEGER NOT NULL)",
		"run_host_summary.failures (INTEGER NOT NULL)",
		"run_host_summary.host (TEXT NOT NULL)",
		"run_host_summary.ok (INTEGER NOT NULL)",
		"run_host_summary.ran_at (TEXT NOT NULL)",
		"run_host_summary.skipped (INTEGER NOT NULL)",
		"run_host_summary.unreachable (INTEGER NOT NULL)",
		"run_host_summary.worst (TEXT NOT NULL)",
		"run_logs.chunk (BLOB NOT NULL)",
		"run_logs.run_id (TEXT NOT NULL)",
		"run_task_summary.ran_at (TEXT NOT NULL)",
		"run_task_summary.seconds (REAL NOT NULL)",
		"run_task_summary.task (TEXT NOT NULL)",
		"runs.created_at (TEXT NOT NULL)",
		"runs.inventory (TEXT NOT NULL)",
		"runs.playbook (TEXT NOT NULL)",
		"runs.status (TEXT NOT NULL)",
		"schedules.created_at (TEXT NOT NULL)",
		"schedules.cron (TEXT NOT NULL)",
		"team_members.user_id (TEXT NOT NULL)",
		"teams.created_at (TEXT NOT NULL)",
		"templates.created_at (TEXT NOT NULL)",
		"templates.playbook (TEXT NOT NULL)",
		"tokens.created_at (TEXT NOT NULL)",
		"tokens.hash (TEXT NOT NULL)",
		"triggers.created_at (TEXT NOT NULL)",
		"triggers.template_id (TEXT NOT NULL)",
		"triggers.token_hash (TEXT NOT NULL)",
		"users.created_at (TEXT NOT NULL)",
		"users.password_hash (TEXT NOT NULL)",
		"users.role (TEXT NOT NULL)",
		"users.username (TEXT NOT NULL)",
	}
	sort.Strings(wantNonAddable)
	if diff := cmp.Diff(wantNonAddable, violations); diff != "" {
		t.Errorf("the schema's non-addable columns changed (-known +now):\n%s\n"+
			"A new entry means a column ALTER cannot add: give it a DEFAULT so the healer covers "+
			"upgraded databases, or write a hand migration and record it in the allowlist in this test.",
			diff)
	}
	_ = allowed
}
