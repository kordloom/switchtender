package server

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// hardenDB is the stores of one database a hardening test serves from.
type hardenDB struct {
	// runs holds runs and the shared budgets.
	runs run.Store
	// users holds accounts.
	users user.Store
	// tokens holds API tokens.
	tokens auth.Store
	// inventories holds inventories.
	inventories inventory.Store
	// templates holds templates.
	templates template.Store
	// teams holds teams and their members.
	teams team.Store
	// orgs holds organizations and their members.
	orgs org.Store
	// grants holds grants.
	grants grant.Store
	// projects holds projects.
	projects project.Store
	// credentials holds credentials.
	credentials credential.Store
	// schedules holds schedules.
	schedules schedule.Store
}

// hardenBackend opens databases of one kind for a hardening test.
type hardenBackend struct {
	// Name labels the backend in subtest names.
	Name string
	// Open returns the stores of a fresh database and a function that opens the same database
	// again, the way a second replica opens the database the first one uses.
	Open func(t *testing.T) (hardenDB, func() hardenDB)
}

// hardenBackends returns the backends a hardening test runs on: the in-memory stores when
// withMemory is set, SQLite, and PostgreSQL when the test DSN names a server.
func hardenBackends(withMemory bool) []hardenBackend {
	var out []hardenBackend
	if withMemory {
		out = append(out, hardenBackend{Name: "memory", Open: openMemoryHarden})
	}
	return append(out, hardenBackend{Name: "sqlite", Open: openSQLiteHarden},
		hardenBackend{Name: "postgres", Open: openPostgresHarden})
}

// openMemoryHarden returns in-memory stores, which a second replica shares as they are.
func openMemoryHarden(_ *testing.T) (hardenDB, func() hardenDB) {
	db := hardenDB{runs: run.NewMemStore(), users: user.NewMemStore(), tokens: auth.NewMemStore(),
		inventories: inventory.NewMemStore(), templates: template.NewMemStore(),
		teams: team.NewMemStore(), orgs: org.NewMemStore(), grants: grant.NewMemStore(),
		projects: project.NewMemStore(), credentials: credential.NewMemStore(),
		schedules: schedule.NewMemStore()}
	return db, func() hardenDB { return db }
}

// openSQLiteHarden returns the stores of a fresh SQLite file, and opens the file again for a second
// replica.
func openSQLiteHarden(t *testing.T) (hardenDB, func() hardenDB) {
	path := filepath.Join(t.TempDir(), "switchtender.db")
	open := func() hardenDB {
		db, err := sqlitestore.Open(path)
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return hardenDB{runs: db.Runs(), users: db.Users(), tokens: db.Tokens(),
			inventories: db.Inventories(), templates: db.Templates(), teams: db.Teams(),
			orgs: db.Orgs(), grants: db.Grants(), projects: db.Projects(),
			credentials: db.Credentials(), schedules: db.Schedules()}
	}
	return open(), open
}

// openPostgresHarden returns the stores of a PostgreSQL database of the test's own, and opens it
// again for a second replica. It skips where no server is named, or fails where the full suite was
// demanded.
func openPostgresHarden(t *testing.T) (hardenDB, func() hardenDB) {
	if os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN") == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
		}
		t.Skip("set SWITCHTENDER_TEST_POSTGRES_DSN to run the PostgreSQL case")
	}
	dsn := cbkFreshDatabase(t)
	open := func() hardenDB {
		db, err := pgstore.Open(dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return hardenDB{runs: db.Runs(), users: db.Users(), tokens: db.Tokens(),
			inventories: db.Inventories(), templates: db.Templates(), teams: db.Teams(),
			orgs: db.Orgs(), grants: db.Grants(), projects: db.Projects(),
			credentials: db.Credentials(), schedules: db.Schedules()}
	}
	return open(), open
}

// hardenServer serves db with every store a hardening test reaches, and returns the handler and an
// unscoped admin token for it.
func hardenServer(t *testing.T, db hardenDB) (http.Handler, string) {
	t.Helper()
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(db.runs, runner, zap.NewNop(), dispatch.WithInventories(db.inventories),
		dispatch.WithNoJanitor(), dispatch.WithQueues([]string{"harden-unserved"}),
		dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	plain, tok, err := auth.New("harden-admin")
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if err := db.tokens.Save(context.Background(), tok); err != nil {
		t.Fatalf("save token: %v", err)
	}
	h := New(db.runs, d, zap.NewNop(), WithUsers(db.users), WithTokens(db.tokens),
		WithInventories(db.inventories), WithTemplates(db.templates), WithTeams(db.teams),
		WithOrgs(db.orgs), WithGrants(db.grants, false), WithProjects(db.projects),
		WithCredentials(db.credentials, credential.NewSealer("harden-pass", "harden-salt")),
		WithSchedules(db.schedules), WithAudit(&recordingAudits{})).Handler()
	return h, plain
}

// hardenCall sends one request to h as the bearer of token and returns the recorded answer.
func hardenCall(h http.Handler, token, method, target, body string,
	header map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// randomText returns n hex characters from a generator seeded with seed, text PostgreSQL cannot
// compress below its index entry limit the way it compresses a repeated character.
func randomText(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	const hexDigits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[r.IntN(len(hexDigits))]
	}
	return string(b)
}
