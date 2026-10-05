package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Registers the driver the own-database helper opens.
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// cbkNative is the callback template's own callback address.
const cbkNative = "/v1/templates/tpl_cb/callback"

// cbkStores are the stores two replicas share, the way two servers share one database.
type cbkStores struct {
	// name labels the backend in subtest names.
	name string
	// runs holds the runs both replicas launch and check for pending callbacks.
	runs run.Store
	// templates holds the callback template and its sealed key.
	templates template.Store
	// inventories holds the host list callers are matched against.
	inventories inventory.Store
}

// cbkMemoryStores returns stores two in-process replicas share, the way two servers share one
// database.
func cbkMemoryStores() cbkStores {
	return cbkStores{name: "memory", runs: run.NewMemStore(), templates: template.NewMemStore(),
		inventories: inventory.NewMemStore()}
}

// cbkPostgresStores returns the stores of a PostgreSQL database of the test's own, skipping the
// test where no server is named, or failing it where the full suite was demanded.
func cbkPostgresStores(t *testing.T) cbkStores {
	t.Helper()
	if os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN") == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
		}
		t.Skip("set SWITCHTENDER_TEST_POSTGRES_DSN to run the PostgreSQL callback test")
	}
	db, err := pgstore.Open(cbkFreshDatabase(t))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return cbkStores{name: "postgres", runs: db.Runs(), templates: db.Templates(),
		inventories: db.Inventories()}
}

// cbkFreshDatabase creates a database of this test's own on the server the test DSN names and
// returns a DSN for it, dropping it when the test ends, since the store tests truncate the shared
// one.
func cbkFreshDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// The time alone is not unique: two subtests reach this at the same instant on a clock with
	// microsecond resolution, and the second CREATE DATABASE then fails on the name.
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("read a random suffix: %v", err)
	}
	name := fmt.Sprintf("st_cbk_%d_%x", time.Now().UnixNano(), suffix)
	if _, err := conn.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create a database of this test's own: %v", err)
	}
	t.Cleanup(func() {
		c, cerr := sql.Open("pgx", dsn)
		if cerr != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// cbkSeed stores the callback inventory and a template accepting callbacks, the way an install
// holds them before any replica starts.
func cbkSeed(t *testing.T, s cbkStores) {
	t.Helper()
	ctx := context.Background()
	if err := s.inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: callbackInventory, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := s.templates.Save(ctx, &template.Template{ID: "tpl_cb", Name: "boot",
		Playbook: "boot.yml", InventoryID: "inv_1", Limit: "web", AllowCallbacks: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save template: %v", err)
	}
}

// cbkGate wraps a replica's submitter and, while armed, holds the first submit it is given until
// released, so a test can stop one replica after its pending check and dedupe lookup and before
// its run is saved.
type cbkGate struct {
	// next is the submitter the call is passed to once released.
	next Submitter
	// armed holds the next submit when true.
	armed bool
	// entered is closed once a held submit has arrived.
	entered chan struct{}
	// release lets the held submit continue.
	release chan struct{}
	// key is the idempotency key the held submit carries.
	key string
	// mu guards armed and key.
	mu sync.Mutex
}

// Submit holds the call while the gate is armed, recording the key it carries, then submits it.
func (g *cbkGate) Submit(ctx context.Context, playbook, inventory string,
	opts ...run.SubmitOption) (*run.Run, error) {
	g.mu.Lock()
	hold := g.armed
	g.armed = false
	if hold {
		probe := &run.Run{}
		run.ApplyOptions(probe, opts)
		g.key = probe.IdempotencyKey
	}
	g.mu.Unlock()
	if hold {
		close(g.entered)
		<-g.release
	}
	return g.next.Submit(ctx, playbook, inventory, opts...)
}

// SubmitSplit passes the call on.
func (g *cbkGate) SubmitSplit(ctx context.Context, playbook, inventory string, shards int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return g.next.SubmitSplit(ctx, playbook, inventory, shards, opts...)
}

// SubmitPipeline passes the call on.
func (g *cbkGate) SubmitPipeline(ctx context.Context, name, inventory string,
	steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	return g.next.SubmitPipeline(ctx, name, inventory, steps, opts...)
}

// cbkReplica is one server process on the shared stores: its own dispatcher, rate limits, and
// launch lock, as a second replica behind a load balancer has.
type cbkReplica struct {
	// handler is the replica's whole handler.
	handler http.Handler
	// gate is the replica's submitter.
	gate *cbkGate
}

// cbkNewReplica starts a replica on s. Its dispatcher serves a queue no run targets, so every
// callback run stays pending and visible to the pending check for as long as the test runs.
func cbkNewReplica(t *testing.T, s cbkStores) *cbkReplica {
	t.Helper()
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(s.runs, runner, zap.NewNop(), dispatch.WithInventories(s.inventories),
		dispatch.WithNoJanitor(), dispatch.WithQueues([]string{"cbk-unserved"}),
		dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	gate := &cbkGate{next: d, entered: make(chan struct{}), release: make(chan struct{})}
	h := New(s.runs, gate, zap.NewNop(), WithTemplates(s.templates),
		WithInventories(s.inventories), WithAudit(&recordingAudits{}),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(srv *Server) { srv.callbackResolver = fakeResolver{} },
		WithCallbackLimitMatcher(webLimit())).Handler()
	return &cbkReplica{handler: h, gate: gate}
}

// mint mints the template's callback key through this replica and returns its plaintext.
func (r *cbkReplica) mint(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_cb/callback-key", nil)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint key status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp callbackKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.HostConfigKey == "" {
		t.Fatalf("mint key response %s: %v", rec.Body.String(), err)
	}
	return resp.HostConfigKey
}

// call posts web01's callback with key to path on this replica.
func (r *cbkReplica) call(path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(keyBody(key)))
	req.RemoteAddr = "10.0.0.11:40000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// cbkBucket reads the dedupe bucket number a derived idempotency key ends with.
func cbkBucket(t *testing.T, key string) int64 {
	t.Helper()
	at := strings.LastIndexByte(key, ':')
	if at == -1 {
		t.Fatalf("the callback submitted with the key %q, which carries no dedupe bucket", key)
	}
	n, err := strconv.ParseInt(key[at+1:], 10, 64)
	if err != nil {
		t.Fatalf("the callback key %q ends with %q, not a bucket number", key, key[at+1:])
	}
	return n
}

// cbkPendingCallbackRuns returns the ids of the unfinished callback runs for web01.
func cbkPendingCallbackRuns(t *testing.T, store run.Store) []string {
	t.Helper()
	live, err := store.NonTerminal(context.Background())
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	var ids []string
	for _, r := range live {
		if r.Source == callbackSource && r.SourceID == "tpl_cb" && r.Limit == "web01" {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// TestCallbackOnTwoReplicasLaunchesTwiceAcrossADedupeBucket races one host's callback on two
// replicas sharing one database, the deployment high availability documents.
//
// The promise, in docs/migration.md and docs/api.md, is that a second callback while a run for the
// host is pending or running answers 409, and the handler's own comment says the dedupe key closes
// the race across replicas that its in-process lock closes on one. The pending check reads the run
// table and the save comes later, with the chain append between them, so the key is the only
// cross-replica guard, and it is a ten-second wall-clock bucket the handler reads from its own
// clock. Replica A is held after its pending check and dedupe lookup, before its run is saved, the
// window every callback passes through. Replica B's callback for the same host lands in the next
// bucket, finds nothing pending and no key in its window, and launches. A then saves under its own
// bucket's key, which nothing holds, and launches too: two provisioning runs against one host at
// once. Within one bucket the unique index collapses A onto B's run, so the boundary is the whole
// trigger, and replica clocks a bucket apart turn every overlapping pair into a double launch.
func TestCallbackOnTwoReplicasLaunchesTwiceAcrossADedupeBucket(t *testing.T) {
	t.Parallel()
	stores := cbkMemoryStores()
	cbkSeed(t, stores)
	a, b := cbkNewReplica(t, stores), cbkNewReplica(t, stores)
	key := a.mint(t)

	a.gate.mu.Lock()
	a.gate.armed = true
	a.gate.mu.Unlock()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- a.call(cbkNative, key) }()
	select {
	case <-a.gate.entered:
	case rec := <-first:
		t.Fatalf("replica A answered %d before submitting: %s", rec.Code, rec.Body.String())
	}
	bucket := cbkBucket(t, a.gate.key)
	for time.Now().UnixNano()/int64(run.DedupeWindow) <= bucket {
		time.Sleep(10 * time.Millisecond)
	}
	second := b.call(cbkNative, key)
	close(a.gate.release)
	firstRec := <-first

	ids := cbkPendingCallbackRuns(t, stores.runs)
	if len(ids) != 1 {
		t.Fatalf("one host's callback raced on two replicas left %d unfinished callback runs for "+
			"web01, %v, want 1: replica A answered %d %s, replica B answered %d %s", len(ids), ids,
			firstRec.Code, strings.TrimSpace(firstRec.Body.String()), second.Code,
			strings.TrimSpace(second.Body.String()))
	}
}

// TestCallbackOnPostgreSQLLaunches sends one host's provisioning callback, on the template's own
// address and on its AWX-compatible address, to a server whose database is PostgreSQL, the
// backend high availability runs on.
//
// docs/api.md promises a callback with the right key from a matched host launches the template and
// answers 201. The replay guard derives its dedupe key from the template id and the host joined by
// a NUL byte, and PostgreSQL refuses a NUL in a text parameter with SQLSTATE 22021, so the lookup
// fails and every callback answers 500 "could not launch the template" before anything is
// recorded or launched. A host that boots against a PostgreSQL install is never provisioned, on
// either address, and the migration scenario for callbacks runs on SQLite only, where a NUL is
// stored without complaint.
func TestCallbackOnPostgreSQLLaunches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path string
	}{{ // Test 0: The template's own address.
		Path: cbkNative,
	}, { // Test 1: The AWX-compatible address an import bound.
		Path: "/api/v2/job_templates/42/callback/",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			stores := cbkPostgresStores(t)
			cbkSeed(t, stores)
			ctx := context.Background()
			tpl, err := stores.templates.Get(ctx, "tpl_cb")
			if err != nil {
				t.Fatalf("read the template: %v", err)
			}
			tpl.AWXCallback = true
			if err := stores.templates.Save(ctx, tpl); err != nil {
				t.Fatalf("turn the AWX-compatible address on: %v", err)
			}
			if err := stores.templates.BindAWX(ctx, template.AWXBinding{AWXID: 42,
				TemplateID: "tpl_cb", Name: "boot", CreatedAt: time.Now(),
				UpdatedAt: time.Now()}); err != nil {
				t.Fatalf("bind the AWX id: %v", err)
			}
			replica := cbkNewReplica(t, stores)
			key := replica.mint(t)
			rec := replica.call(test.Path, key)
			if rec.Code != http.StatusCreated {
				t.Fatalf("a callback with the right key from web01 on PostgreSQL = %d %s, want 201",
					rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if ids := cbkPendingCallbackRuns(t, stores.runs); len(ids) != 1 {
				t.Errorf("the callback left %d callback runs for web01, want 1", len(ids))
			}
		})
	}
}

// TestCallbackWrongKeyBudgetHoldsAcrossReplicas spends one client address's wrong-key budget on
// one replica and then guesses again through a second replica on the same database.
//
// docs/configuration.md promises that past --callback-key-failure-limit wrong keys in a minute
// every callback from the address is refused for the rest of the minute, the right key included,
// so a key cannot be guessed at a useful rate. The budget is a map in each process, so behind a
// load balancer the address gets the full budget again on every replica, and again after every
// restart: the second replica checks the next guess against the key instead of refusing it.
func TestCallbackWrongKeyBudgetHoldsAcrossReplicas(t *testing.T) {
	t.Parallel()
	stores := cbkMemoryStores()
	cbkSeed(t, stores)
	a, b := cbkNewReplica(t, stores), cbkNewReplica(t, stores)
	a.mint(t)
	for i := 0; i < DefaultCallbackKeyFailureLimit; i++ {
		rec := a.call(cbkNative, "wrong-key-guess-"+strconv.Itoa(i))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("wrong key %d on replica A = %d, want 403: %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := a.call(cbkNative, "wrong-key-guess-a"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("replica A after the budget = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec := b.call(cbkNative, "wrong-key-guess-b"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("replica B checked a guess from an address whose wrong-key budget is spent: "+
			"status %d, want 429: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
