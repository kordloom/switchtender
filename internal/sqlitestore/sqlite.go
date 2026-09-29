// Package sqlitestore implements run.Store on top of SQLite using the pure Go modernc driver, so
// SwitchTender keeps its single binary promise with no cgo. It is the default backend. A Postgres
// backend can satisfy the same run.Store interface later for multi instance deployments.
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	sqlite "modernc.org/sqlite"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// splitDB routes statements over two pools: every write goes to the single serialized write
// connection, while reads run on a read-only pool so a long read never blocks a write, which WAL
// supports. The read pool is opened with query_only set, so a statement misrouted to it fails loudly
// instead of racing the writer into SQLite's read-to-write upgrade deadlock.
type splitDB struct {
	// w is the single write connection.
	w *sql.DB
	// r is the read-only pool. It equals w when the path does not support a second handle, such as an
	// in-memory database.
	r *sql.DB
}

// ExecContext runs a write statement on the write connection.
func (d *splitDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.w.ExecContext(ctx, query, args...)
}

// BeginTx starts a transaction on the write connection.
func (d *splitDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return d.w.BeginTx(ctx, opts)
}

// QueryContext runs a read query on the read pool.
func (d *splitDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.r.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row read query on the read pool.
func (d *splitDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.r.QueryRowContext(ctx, query, args...)
}

// writeQueryRowContext runs a single-row statement on the write connection, for a write that returns
// its row, such as a claim's UPDATE RETURNING.
func (d *splitDB) writeQueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.w.QueryRowContext(ctx, query, args...)
}

// Close closes both pools.
func (d *splitDB) Close() error {
	err := d.w.Close()
	if d.r != d.w {
		err = errors.Join(err, d.r.Close())
	}
	return err
}

// store is a run.Store backed by a SQLite database.
type store struct {
	// db is the open database handle.
	db *splitDB
}

// scanner is the read side shared by sql.Row and sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// DB holds the SQLite backed run and schedule stores sharing one database.
type DB struct {
	// db is the open database handle.
	db *splitDB
	// runs is the run store.
	runs *store
	// schedules is the schedule store.
	schedules *scheduleStore
	// tokens is the API token store.
	tokens *tokenStore
	// credentials is the execution secret store.
	credentials *credentialStore
	credTypes   *credTypeStore
	// projects is the git project store.
	projects *projectStore
	// templates is the job template store.
	templates *templateStore
	// users is the account store.
	users *userStore
	// inventories is the stored inventory store.
	inventories *inventoryStore
	// audits is the audit trail store.
	audits *auditStore
	// invSources is the dynamic inventory source store.
	invSources *invSourceStore
	// triggers is the webhook trigger store.
	triggers *triggerStore
	// teams is the team store.
	teams *teamStore
	// orgs is the organization store.
	orgs *orgStore
	// grants is the per-object access grant store.
	grants *grantStore
	// policies is the approval policy store.
	policies *policyStore
}

// sqliteConstraintUnique and sqliteConstraintPrimaryKey are the extended result codes for the two
// constraint classes a duplicate key can trip. Every other constraint class is a different fault and
// must not be reported as a key collision.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// isKeyConflict reports whether a keyed insert failed because another run already holds the
// idempotency key.
//
// It matches the unique-constraint code specifically, the way the Postgres side matches 23505.
// Accepting any constraint class meant a NOT NULL, CHECK, or foreign-key violation on a keyed insert
// came back as "another run already holds this key", which is a wrong answer that reads as a normal
// race: the caller is handed somebody else's run instead of an error naming the real problem. The
// two backends now classify the same failure the same way.
func isKeyConflict(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code() == sqliteConstraintUnique || serr.Code() == sqliteConstraintPrimaryKey
}

// Runs returns the run store.
func (d *DB) Runs() run.Store {
	return d.runs
}

// Schedules returns the schedule store.
func (d *DB) Schedules() schedule.Store {
	return d.schedules
}

// Tokens returns the API token store.
func (d *DB) Tokens() auth.Store {
	return d.tokens
}

// Credentials returns the execution secret store.
func (d *DB) Credentials() credential.Store {
	return d.credentials
}

// CredentialTypes returns the operator-defined credential type store.
func (d *DB) CredentialTypes() credential.TypeStore {
	return d.credTypes
}

// Projects returns the git project store.
func (d *DB) Projects() project.Store {
	return d.projects
}

// Templates returns the job template store.
func (d *DB) Templates() template.Store {
	return d.templates
}

// Users returns the account store.
func (d *DB) Users() user.Store {
	return d.users
}

// Inventories returns the stored inventory store.
func (d *DB) Inventories() inventory.Store {
	return d.inventories
}

// Policies returns the approval policy store.
func (d *DB) Policies() policy.Store {
	return d.policies
}

// Audits returns the audit trail store.
func (d *DB) Audits() audit.Store {
	return d.audits
}

// InventorySources returns the dynamic inventory source store.
func (d *DB) InventorySources() invsource.Store {
	return d.invSources
}

// Triggers returns the webhook trigger store.
func (d *DB) Triggers() trigger.Store {
	return d.triggers
}

// Teams returns the team store.
func (d *DB) Teams() team.Store {
	return d.teams
}

// Orgs returns the organization store.
func (d *DB) Orgs() org.Store {
	return d.orgs
}

// Grants returns the per-object access grant store.
func (d *DB) Grants() grant.Store {
	return d.grants
}

// Close closes the underlying database.
func (d *DB) Close() error {
	return d.db.Close()
}
