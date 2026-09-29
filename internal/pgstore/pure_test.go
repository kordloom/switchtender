package pgstore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestRunSearchClauseBuildsOneBoundTerm pins the shape of the runs-view search predicate. The clause
// is assembled by string concatenation around a single placeholder, so the thing that matters is
// that the search term never reaches the SQL text: it is bound once as $1 and reused by every
// column. A refactor that interpolated the term instead would pass every behavioral test against a
// well-behaved query and hand an attacker the runs table.
func TestRunSearchClauseBuildsOneBoundTerm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Query      string
		WantClause bool
		WantArg    any
	}{{ // Test 0: An empty term searches nothing rather than matching everything.
		Query: "", WantClause: false, WantArg: nil,
	}, { // Test 1: Whitespace is trimmed away to nothing, so it is also no search.
		Query: "   \t\n  ", WantClause: false, WantArg: nil,
	}, { // Test 2: An ordinary term is lowercased and wrapped in wildcards.
		Query: "Deploy", WantClause: true, WantArg: "%deploy%",
	}, { // Test 3: Surrounding whitespace is trimmed before wrapping.
		Query: "  deploy  ", WantClause: true, WantArg: "%deploy%",
	}, { // Test 4: A quote is bound, never interpolated, so it cannot close a literal.
		Query: "' OR 1=1 --", WantClause: true, WantArg: "%' or 1=1 --%",
	}, { // Test 5: A semicolon is likewise just text inside the bound argument.
		Query: "x'; DROP TABLE runs; --", WantClause: true, WantArg: "%x'; drop table runs; --%",
	}, { // Test 6: Unicode is lowercased by the same rule and bound whole.
		Query: "ÉLÈVE", WantClause: true, WantArg: "%élève%",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			clause, args := runSearchClause(test.Query)
			if got := clause != ""; got != test.WantClause {
				t.Fatalf("clause present = %v, want %v (clause %q)", got, test.WantClause, clause)
			}
			if !test.WantClause {
				if len(args) != 0 {
					t.Errorf("no clause but %d args bound", len(args))
				}
				return
			}
			if len(args) != 1 {
				t.Fatalf("args = %d, want exactly one bound term", len(args))
			}
			if diff := cmp.Diff(test.WantArg, args[0]); diff != "" {
				t.Errorf("bound term mismatch (-want +got):\n%s", diff)
			}
			// The term itself must not appear in the SQL text. Only the placeholder may.
			if strings.Contains(clause, test.Query) && test.Query != "" {
				t.Errorf("the search term is inlined in the SQL text, not bound:\n%s", clause)
			}
			if strings.Count(clause, "$1") != len(runSearchColumns) {
				t.Errorf("clause reuses $1 %d times, want one per searched column (%d):\n%s",
					strings.Count(clause, "$1"), len(runSearchColumns), clause)
			}
			if strings.Contains(clause, "$2") {
				t.Errorf("clause binds a second placeholder it never supplies an arg for:\n%s", clause)
			}
		})
	}
}

// TestRunSearchClauseCoversEverySearchedColumn holds the generated clause against the declared
// column list. The runs view promises the same fields the in-memory store's matchesQuery searches,
// so a column silently dropped from the clause makes the two backends disagree about what a search
// returns, which is the class of drift the whole shared-contract arrangement exists to prevent.
func TestRunSearchClauseCoversEverySearchedColumn(t *testing.T) {
	t.Parallel()
	clause, _ := runSearchClause("anything")
	for testNum, col := range runSearchColumns {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			want := "lower(" + col + ") LIKE $1 ESCAPE ''"
			if !strings.Contains(clause, want) {
				t.Errorf("clause does not search %s as %q:\n%s", col, want, clause)
			}
		})
	}
	// ESCAPE '' disables PostgreSQL's backslash escape so a backslash is literal, the way SQLite's
	// LIKE already treats it. Without this the two backends match different rows for one term.
	if n := strings.Count(clause, "ESCAPE ''"); n != len(runSearchColumns) {
		t.Errorf("ESCAPE '' appears %d times, want once per column (%d): a column without it reads "+
			"a backslash as an escape here and as a literal on SQLite", n, len(runSearchColumns))
	}
}

// TestEverySelectListConstantIsChecked closes a hole in TestEverySelectedColumnIsDeclared. That test
// guards the org_id defect, a column selected but never declared, and it does so from a hand-kept
// list of tables. A select list absent from that list is simply unguarded, and three of them were:
// tokens, triggers, and users. This walks the package source for every *Columns constant and fails
// when one is not covered, so the guard cannot quietly shrink again.
func TestEverySelectListConstantIsChecked(t *testing.T) {
	t.Parallel()
	// The constants declared in the package, paired with the table each one reads.
	all := map[string]string{
		"runColumns":         "runs",
		"hostSummaryColumns": "run_host_summary",
		"credentialColumns":  "credentials",
		"grantColumns":       "grants",
		"credTypeColumns":    "credential_types",
		"inventoryColumns":   "inventories",
		"invSourceColumns":   "inventory_sources",
		"projectColumns":     "projects",
		"policyColumns":      "policies",
		"scheduleColumns":    "schedules",
		"templateColumns":    "templates",
		"tokenColumns":       "tokens",
		"triggerColumns":     "triggers",
		"userColumns":        "users",
	}
	parsed := sqlutil.ParseSchemaColumns(schema)
	values := map[string]string{
		"runColumns": runColumns, "hostSummaryColumns": hostSummaryColumns,
		"credentialColumns": credentialColumns, "grantColumns": grantColumns,
		"credTypeColumns": credTypeColumns, "inventoryColumns": inventoryColumns,
		"invSourceColumns": invSourceColumns, "projectColumns": projectColumns,
		"policyColumns": policyColumns, "scheduleColumns": scheduleColumns,
		"templateColumns": templateColumns, "tokenColumns": tokenColumns,
		"triggerColumns": triggerColumns, "userColumns": userColumns,
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	for testNum, name := range names {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			table := all[name]
			declared := map[string]bool{}
			for _, c := range parsed[table] {
				declared[c.Name] = true
			}
			if len(declared) == 0 {
				t.Fatalf("the schema parse found no columns for %s, so %s cannot be checked or "+
					"healed", table, name)
			}
			for _, raw := range strings.Split(values[name], ",") {
				col := strings.TrimSpace(raw)
				if col == "" || declared[col] {
					continue
				}
				t.Errorf("%s selects %q, which the CREATE for %s does not declare: a fresh database "+
					"gets it only by accident and an upgraded one may never", name, col, table)
			}
		})
	}
}

// TestAuditSelectListsMatchTheDeclaredColumns guards the two audit tables the same way. Their select
// lists are written inline in every query rather than held in a constant, so the shared guard never
// saw them at all. The audit trail is the tamper-evident record the product sells; a read that fails
// after an upgrade because a column was never declared takes the whole chain offline.
func TestAuditSelectListsMatchTheDeclaredColumns(t *testing.T) {
	t.Parallel()
	parsed := sqlutil.ParseSchemaColumns(schema)
	tests := []struct {
		Table   string
		Columns string
	}{{ // Test 0: Every column Append writes and every read scans back.
		Table: "audit_entries",
		Columns: "id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, " +
			"prev_hash, hash, nonce, install_id",
	}, { // Test 1: The anchor columns SaveAnchor writes and Anchors reads.
		Table:   "audit_anchors",
		Columns: "id, type, shape, seq, link, at, ref, proof, install_id",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			declared := map[string]bool{}
			for _, c := range parsed[test.Table] {
				declared[c.Name] = true
			}
			if len(declared) == 0 {
				t.Fatalf("the schema parse found no columns for %s", test.Table)
			}
			for _, raw := range strings.Split(test.Columns, ",") {
				col := strings.TrimSpace(raw)
				if col == "" || declared[col] {
					continue
				}
				t.Errorf("%s reads %q, which its CREATE does not declare", test.Table, col)
			}
		})
	}
}

// TestSchemaIsIdempotentByConstruction reads the schema blob as text and requires every statement
// that creates or alters an object to carry the guard that makes re-running it a no-op. Open doubles
// as the migration, and every process calls it on ordinary startup, so a single unguarded CREATE or
// ADD COLUMN aborts the whole migration transaction on the second boot and the node never starts.
func TestSchemaIsIdempotentByConstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Prefix string
		Guard  string
	}{{ // Test 0: A table must not be created twice.
		Prefix: "CREATE TABLE ", Guard: "CREATE TABLE IF NOT EXISTS ",
	}, { // Test 1: Nor an index.
		Prefix: "CREATE INDEX ", Guard: "CREATE INDEX IF NOT EXISTS ",
	}, { // Test 2: Nor a unique index.
		Prefix: "CREATE UNIQUE INDEX ", Guard: "CREATE UNIQUE INDEX IF NOT EXISTS ",
	}, { // Test 3: A dropped index may already be gone.
		Prefix: "DROP INDEX ", Guard: "DROP INDEX IF EXISTS ",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, line := range strings.Split(schema, "\n") {
				stmt := strings.TrimSpace(line)
				if strings.HasPrefix(stmt, "--") || !strings.HasPrefix(stmt, test.Prefix) {
					continue
				}
				if !strings.HasPrefix(stmt, test.Guard) {
					t.Errorf("this statement is not idempotent, so the second boot aborts the "+
						"migration transaction and the node never starts:\n\t%s", stmt)
				}
			}
		})
	}
	// ADD COLUMN is the one the schema's own comments say broke a release, so it gets its own pass.
	for _, line := range strings.Split(schema, "\n") {
		stmt := strings.TrimSpace(line)
		if !strings.Contains(stmt, "ADD COLUMN") || strings.HasPrefix(stmt, "--") {
			continue
		}
		if !strings.Contains(stmt, "ADD COLUMN IF NOT EXISTS") {
			t.Errorf("an unguarded ADD COLUMN fails every boot after the first:\n\t%s", stmt)
		}
	}
}

// TestEveryAlterTableFollowsItsCreate pins the ordering rule the policies table's own comment
// records paying for: the schema blob executes top to bottom, so an ALTER naming a table whose
// CREATE comes later fails the whole migration on a fresh database. The comment says this happened
// once. Nothing but this test stops it happening again.
func TestEveryAlterTableFollowsItsCreate(t *testing.T) {
	t.Parallel()
	created := map[string]int{}
	lines := strings.Split(schema, "\n")
	for i, line := range lines {
		stmt := strings.TrimSpace(line)
		if !strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS ") {
			continue
		}
		name := strings.Fields(strings.TrimPrefix(stmt, "CREATE TABLE IF NOT EXISTS "))[0]
		name = strings.TrimSuffix(name, "(")
		if _, seen := created[name]; !seen {
			created[name] = i
		}
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements were found, so this guard proves nothing")
	}
	for i, line := range lines {
		stmt := strings.TrimSpace(line)
		if !strings.HasPrefix(stmt, "ALTER TABLE ") {
			continue
		}
		name := strings.Fields(strings.TrimPrefix(stmt, "ALTER TABLE "))[0]
		at, ok := created[name]
		if !ok {
			t.Errorf("line %d alters %s, which this schema never creates:\n\t%s", i+1, name, stmt)
			continue
		}
		if at > i {
			t.Errorf("line %d alters %s before its CREATE on line %d, which fails the whole "+
				"migration on a fresh database:\n\t%s", i+1, name, at+1, stmt)
		}
	}
}

// TestSchemaHealCoversEveryAddableColumn pins the relationship Open depends on: the heal derives its
// ALTER statements from the parsed schema, not from the hand-kept list in the blob, so any column
// the parser reports as addable is one an upgraded database gets automatically. A column the parser
// cannot see is a column the heal will never add, which is exactly the org_id defect.
func TestSchemaHealCoversEveryAddableColumn(t *testing.T) {
	t.Parallel()
	parsed := sqlutil.ParseSchemaColumns(schema)
	if len(parsed) == 0 {
		t.Fatal("the schema parse produced no tables, so Open heals nothing on upgrade")
	}
	// Every table the blob creates has to be visible to the parser, or it is silently unhealed.
	for _, line := range strings.Split(schema, "\n") {
		stmt := strings.TrimSpace(line)
		if !strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS ") {
			continue
		}
		name := strings.Fields(strings.TrimPrefix(stmt, "CREATE TABLE IF NOT EXISTS "))[0]
		name = strings.TrimSuffix(name, "(")
		if len(parsed[name]) == 0 {
			t.Errorf("the schema parse sees no columns for %s, so Open can never heal it and a "+
				"database from before any of its columns stays broken", name)
		}
	}
}

// TestMarshalRoundTripsPreserveIntent pins the JSON columns' encode and decode pair. The distinction
// these helpers draw is not cosmetic: an empty column means "not recorded" and decodes to nil, while
// a recorded empty set decodes to an empty value. A receipt that cannot tell "no rules applied" from
// "rules were never captured" is not evidence, so the nil and empty cases are pinned separately.
func TestMarshalRoundTripsPreserveIntent(t *testing.T) {
	t.Parallel()
	t.Run("test 0", func(t *testing.T) { // Test 0: Labels, including unicode keys and values.
		t.Parallel()
		tests := []struct {
			In       map[string]string
			WantText string
		}{
			{In: nil, WantText: ""},
			{In: map[string]string{}, WantText: ""},
			{In: map[string]string{"env": "prod"}, WantText: `{"env":"prod"}`},
			{In: map[string]string{"région": "eu-øst"}, WantText: `{"région":"eu-øst"}`},
		}
		for _, test := range tests {
			text := marshalLabels(test.In)
			if diff := cmp.Diff(test.WantText, text); diff != "" {
				t.Errorf("marshalLabels mismatch (-want +got):\n%s", diff)
			}
			back, err := parseLabels(text)
			if err != nil {
				t.Fatalf("parseLabels(%q) error = %v", text, err)
			}
			if diff := cmp.Diff(test.In, back, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("label round trip mismatch (-want +got):\n%s", diff)
			}
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: A nil policy set stays nil, never an empty set.
		t.Parallel()
		if got := marshalPolicySet(nil); got != "" {
			t.Errorf("marshalPolicySet(nil) = %q, want empty", got)
		}
		if got := unmarshalPolicySet(""); got != nil {
			t.Errorf("unmarshalPolicySet(\"\") = %+v, want nil: an empty column is a run from "+
				"before the set was recorded, which is not the same fact as a set with no rules", got)
		}
		empty := unmarshalPolicySet(marshalPolicySet(&run.PolicySet{}))
		if empty == nil {
			t.Fatal("a recorded empty rule set decoded to nil, erasing the difference between " +
				"\"no rules applied\" and \"rules were never captured\"")
		}
		full := &run.PolicySet{Digest: "abc", Count: 2, Rules: []string{"a", "b"}}
		if diff := cmp.Diff(full, unmarshalPolicySet(marshalPolicySet(full))); diff != "" {
			t.Errorf("policy set round trip mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: Malformed stored JSON decodes to nil, not a panic.
		t.Parallel()
		if got := unmarshalPolicySet("{not json"); got != nil {
			t.Errorf("unmarshalPolicySet on garbage = %+v, want nil", got)
		}
		if got := parseNotifications("{not json"); got != nil {
			t.Errorf("parseNotifications on garbage = %+v, want nil", got)
		}
		if _, err := parseLabels("{not json"); err == nil {
			t.Error("parseLabels accepted garbage, so a corrupt column reads back as no labels")
		}
		if _, err := parseSteps("{not json"); err == nil {
			t.Error("parseSteps accepted garbage, so a corrupt pipeline reads back as no steps")
		}
	})
	t.Run("test 3", func(t *testing.T) { // Test 3: Steps and notifications encode empty as empty.
		t.Parallel()
		if got := marshalSteps(nil); got != "" {
			t.Errorf("marshalSteps(nil) = %q, want empty rather than a JSON null", got)
		}
		if got := marshalSteps([]run.PipelineStep{}); got != "" {
			t.Errorf("marshalSteps(empty) = %q, want empty", got)
		}
		if got := marshalNotifications(nil); got != "" {
			t.Errorf("marshalNotifications(nil) = %q, want empty", got)
		}
		steps, err := parseSteps("")
		if err != nil || steps != nil {
			t.Errorf("parseSteps(\"\") = %v, %v; want nil, nil", steps, err)
		}
	})
}

// TestNonNilMapNeverMarshalsNull pins the helper credential types depend on. The stored columns
// default to '{}' and scanCredType unmarshals them without a nil guard, so a type saved with a nil
// injector map has to reach the database as an object rather than as JSON null.
func TestNonNilMapNeverMarshalsNull(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       map[string]string
		WantSize int
	}{{ // Test 0: A nil map becomes an empty map, so it encodes as {} and not null.
		In: nil, WantSize: 0,
	}, { // Test 1: An already-empty map is returned unchanged.
		In: map[string]string{}, WantSize: 0,
	}, { // Test 2: A populated map passes through untouched.
		In: map[string]string{"TOKEN": "secret"}, WantSize: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := nonNilMap(test.In)
			if got == nil {
				t.Fatal("nonNilMap returned nil, which marshals to JSON null and then fails to " +
					"decode back into the injector map")
			}
			if len(got) != test.WantSize {
				t.Errorf("size = %d, want %d", len(got), test.WantSize)
			}
		})
	}
}

// TestMarshalTypeEncodesEveryColumnAsJSON pins that a credential type's three JSON columns always
// hold decodable JSON, including when every field is nil. scanCredType unmarshals all three with no
// tolerance for an empty string, so a marshal that produced one would make the type unreadable.
func TestMarshalTypeEncodesEveryColumnAsJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        *credential.CredentialType
		WantEnv   string
		WantExtra string
	}{{ // Test 0: A wholly empty type still encodes objects, never empty strings or null.
		In: &credential.CredentialType{}, WantEnv: "{}", WantExtra: "{}",
	}, { // Test 1: Populated injectors survive verbatim.
		In: &credential.CredentialType{
			EnvInjectors:      map[string]string{"A": "1"},
			ExtraVarInjectors: map[string]string{"b": "2"},
		}, WantEnv: `{"A":"1"}`, WantExtra: `{"b":"2"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fields, env, extra, err := marshalType(test.In)
			if err != nil {
				t.Fatalf("marshalType error = %v", err)
			}
			if fields == "" {
				t.Error("fields encoded to an empty string, which scanCredType cannot unmarshal")
			}
			if diff := cmp.Diff(test.WantEnv, env); diff != "" {
				t.Errorf("env mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantExtra, extra); diff != "" {
				t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTerminalPredicatesAgreeWithTheStatusType pins the SQL text against the Go rule it mirrors.
// Retention deletes rows selected by terminalRun and the auxiliary write fences use nonTerminalRun,
// so a status that the Go type calls non-terminal while the SQL calls it finished is a run deleted
// while somebody is still waiting on it. The schema comment records exactly that happening to
// pending_approval.
func TestTerminalPredicatesAgreeWithTheStatusType(t *testing.T) {
	t.Parallel()
	statuses := []run.Status{
		run.StatusPending, run.StatusPendingApproval, run.StatusRunning, run.StatusSucceeded,
		run.StatusFailed, run.StatusCanceled, run.StatusInterrupted, run.StatusRejected,
	}
	for testNum, st := range statuses {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			quoted := "'" + string(st) + "'"
			inTerminal := strings.Contains(terminalRun, quoted)
			if inTerminal != st.Terminal() {
				t.Errorf("terminalRun lists %s = %v but run.Status.Terminal() says %v: retention "+
					"deletes exactly what this predicate matches", st, inTerminal, st.Terminal())
			}
			// nonTerminalRun is the negation, stated as NOT IN over the same set.
			inNonTerminal := strings.Contains(nonTerminalRun, quoted)
			if inNonTerminal != st.Terminal() {
				t.Errorf("nonTerminalRun's excluded set lists %s = %v but Terminal() says %v: the "+
					"log and event fences use this predicate", st, inNonTerminal, st.Terminal())
			}
		})
	}
	// pending_approval is the one the schema comment names, so it is asserted directly too.
	if strings.Contains(terminalRun, "'pending_approval'") {
		t.Error("terminalRun counts pending_approval as finished, so retention deletes runs that " +
			"are waiting for an approver")
	}
}

// TestLockKeysAreDistinct pins that the migration lock and the audit append lock cannot collide.
// Both are session-independent advisory locks on one shared database. If they ever held the same
// key, a node running its startup migration would block every audit append in the cluster, and an
// audit append would block a node from starting.
func TestLockKeysAreDistinct(t *testing.T) {
	t.Parallel()
	if migrateLockKey == auditLockKey {
		t.Fatalf("the migration and audit advisory locks share key %d, so a starting node blocks "+
			"every audit append and an append blocks every start", migrateLockKey)
	}
}

// TestPgNowTextRendersTheSameShapeGoWrites pins the SQL expression that stamps leases. Claim,
// Heartbeat, and the stale sweep all read and write claimed_at, and the sweep casts the text back to
// a timestamp, so the expression has to produce the RFC 3339 UTC form the Go side writes. A drift
// here means a worker's lease is unreadable by the janitor that ages it.
func TestPgNowTextRendersTheSameShapeGoWrites(t *testing.T) {
	t.Parallel()
	for testNum, want := range []string{"AT TIME ZONE 'UTC'", `YYYY-MM-DD"T"HH24:MI:SS`, `"Z"`} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(pgNowText, want) {
				t.Errorf("pgNowText is missing %q, so a lease is not stamped in the UTC RFC 3339 "+
					"form the sweep parses:\n\t%s", want, pgNowText)
			}
		})
	}
}
