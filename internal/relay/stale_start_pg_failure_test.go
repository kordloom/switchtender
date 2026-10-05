package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
)

// rlyPostgresDSNEnv names the PostgreSQL server the two-replica test creates its own database on.
const rlyPostgresDSNEnv = "SWITCHTENDER_TEST_POSTGRES_DSN"

// rlyFreshPostgres creates a database of this test's own on the server rlyPostgresDSNEnv names
// and returns a DSN for it, dropping it when the test ends. It skips without a server, and fails
// instead when SWITCHTENDER_REQUIRE_FULL_SUITE demands the full suite.
func rlyFreshPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(rlyPostgresDSNEnv)
	if dsn == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and %s is not", rlyPostgresDSNEnv)
		}
		t.Skipf("set %s to run the two-replica PostgreSQL scenario", rlyPostgresDSNEnv)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", rlyPostgresDSNEnv, err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("st_rly_stale_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create the test database: %v", err)
	}
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", rlyPostgresDSNEnv, err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestRelayStaleClaimantCannotStartARequeuedRunOnPostgres is the stalled claimant on the high
// availability shape: two control node replicas on one PostgreSQL database. A worker claims a run
// through replica A and receives its secrets, stalls past its lease, and the janitor on replica B
// requeues the run. The worker wakes and starts the run through replica A under the lease B took
// back, and A accepts it and later revokes what the delivery minted while the run executes.
//
// It does not run in parallel: initializing a PostgreSQL schema is a licensed act, and the process
// license this test sets to Team is restored when it ends.
func TestRelayStaleClaimantCannotStartARequeuedRunOnPostgres(t *testing.T) {
	dsn := rlyFreshPostgres(t)
	prev := license.Current()
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_relay_stale", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	t.Cleanup(func() { license.Set(prev) })
	ctx := context.Background()
	replicaA, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica A error = %v", err)
	}
	t.Cleanup(func() { _ = replicaA.Close() })
	replicaB, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica B error = %v", err)
	}
	t.Cleanup(func() { _ = replicaB.Close() })
	if err := replicaA.Runs().Save(ctx, &run.Run{
		ID: "run_stale_pg", Playbook: "site.yml", Status: run.StatusPending, Queue: "dmz",
		CredentialIDs: []string{"cred_fleet"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	opener := newScriptedOpener()
	ts := httptest.NewServer(NewHandler(replicaA.Runs(), &Pools{pools: []Pool{keyedPool(key)}}, nil,
		nil, replicaA.Audits(), WithSecretOpener(opener)))
	t.Cleanup(ts.Close)
	worker, ok := NewHTTPTransport(ts.URL, deliveryToken, nil).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}

	leased, err := worker.Claim(ctx, "relay-a", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if n, err := replicaB.Runs().ReclaimStale(ctx, -time.Minute); err != nil || n != 1 {
		t.Fatalf("replica B ReclaimStale() = %d, %v, want the stale claim requeued", n, err)
	}
	moved, err := worker.Start(ctx, leased.ID, "relay-a", time.Now())
	if err != nil || !moved {
		return
	}
	t.Errorf("replica A accepted a fenced start under a lease replica B's janitor had taken back")

	// Another worker's claim reaches replica A and runs its held-release sweep.
	other, ok := NewHTTPTransport(ts.URL, deliveryToken, nil).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}
	if _, err := other.Claim(ctx, "relay-b", []string{"dmz"}); !errors.Is(err, run.ErrNonePending) {
		t.Fatalf("second worker Claim() error = %v, want nothing pending", err)
	}
	select {
	case id := <-opener.released:
		current, gerr := replicaB.Runs().Get(ctx, id)
		if gerr != nil {
			t.Fatalf("Get() error = %v", gerr)
		}
		if current.Status == run.StatusRunning {
			t.Errorf("replica A revoked what %s's delivery minted while %s was executing it",
				id, current.ClaimedBy)
		}
	case <-time.After(5 * time.Second):
	}
}
