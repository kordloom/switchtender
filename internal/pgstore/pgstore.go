// Package pgstore implements run.Store and schedule.Store on PostgreSQL for multi instance
// deployments. It mirrors the SQLite backend behind the same interfaces and the same shared
// contract tests, storing times as RFC3339 text so both backends order and compare identically.
package pgstore

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// store is a run.Store backed by a PostgreSQL database.
type store struct {
	// db is the open database handle.
	db *sql.DB
}

// scanner is the read side shared by sql.Row and sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// DB holds the PostgreSQL backed run and schedule stores sharing one database.
type DB struct {
	// db is the open database handle.
	db *sql.DB
	// pin lets a read snapshot hold the pool's one connection inside its transaction. See
	// BeginReadSnapshot.
	pin *readPin
	// runs is the run store.
	runs *store
	// schedules is the schedule store.
	schedules *scheduleStore
	// tokens is the API token store.
	tokens *tokenStore
	// credentials is the execution secret store.
	credentials *credentialStore
	credTypes   *credTypeStore
	// fedKeys is the workload identity federation signing key store.
	fedKeys *fedKeyStore
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
	// notifications is the named notification target store.
	notifications *notificationStore
	// teams is the team store.
	teams *teamStore
	// orgs is the organization store.
	orgs *orgStore
	// grants is the per-object access grant store.
	grants *grantStore
	// policies is the approval policy store.
	policies *policyStore
	// factCache is the per-host Ansible fact cache store.
	factCache *factCacheStore
	// reviews is the pull request review report store.
	reviews *reviewStore
}

// and maps to run.ErrDuplicateKey.
func isKeyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// isConstraintConflict reports whether err is a unique violation of the named constraint, for a
// table whose rows other unique indexes also guard, where only one of them means "already there".
func isConstraintConflict(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == constraint
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

// FederationKeys returns the workload identity federation signing key store.
func (d *DB) FederationKeys() federation.KeyStore {
	return d.fedKeys
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

// FactCache returns the per-host Ansible fact cache store.
func (d *DB) FactCache() factcache.Store {
	return d.factCache
}

// ReviewReports returns the pull request review report store.
func (d *DB) ReviewReports() review.Store {
	return d.reviews
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

// Notifications returns the named notification target store.
func (d *DB) Notifications() notification.Store {
	return d.notifications
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
