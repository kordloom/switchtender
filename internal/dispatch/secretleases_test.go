package dispatch_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	_ "github.com/jackc/pgx/v5/stdlib" // Registers the driver the own-database helper opens.
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// fakeVault is a Vault that mints a numbered dynamic secret on each read and counts the revokes of
// each lease it issued.
type fakeVault struct {
	// srv serves the Vault API.
	srv *httptest.Server
	// mu guards minted and revoked.
	mu sync.Mutex
	// minted counts the secrets issued.
	minted int
	// revoked counts the revokes each lease id received.
	revoked map[string]int
}

// newFakeVault starts a fake Vault that answers only the token hvs.lease-test.
func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	v := &fakeVault{revoked: map[string]int{}}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "hvs.lease-test" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/database/creds/app":
			v.minted++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"lease_id":       fmt.Sprintf("database/creds/app/lease-%d", v.minted),
				"lease_duration": 3600,
				"data":           map[string]any{"password": fmt.Sprintf("pw-%d", v.minted)},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sys/leases/revoke":
			var body struct {
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			v.revoked[body.LeaseID]++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// revokes returns how many revokes each lease received.
func (v *fakeVault) revokes() map[string]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]int, len(v.revoked))
	for k, n := range v.revoked {
		out[k] = n
	}
	return out
}

// leaseBackend opens run stores of one kind for the secret lease tests.
type leaseBackend struct {
	// Name labels the backend in subtest names.
	Name string
	// Open returns a fresh store.
	Open func(t *testing.T) run.Store
}

// leaseBackends returns the in-memory store, SQLite, and PostgreSQL when the test DSN names a
// server.
func leaseBackends() []leaseBackend {
	return []leaseBackend{{Name: "memory",
		Open: func(*testing.T) run.Store { return run.NewMemStore() }},
		{Name: "sqlite", Open: func(t *testing.T) run.Store {
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Runs()
		}},
		{Name: "postgres", Open: func(t *testing.T) run.Store {
			db, err := pgstore.Open(leaseFreshPostgres(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Runs()
		}}}
}

// leaseFreshPostgres creates a PostgreSQL database of the test's own and returns its DSN, skipping
// where no server is named and failing where the full suite was demanded.
func leaseFreshPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
		}
		t.Skip("set SWITCHTENDER_TEST_POSTGRES_DSN to run the PostgreSQL case")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer func() { _ = conn.Close() }()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("read a random suffix: %v", err)
	}
	name := fmt.Sprintf("st_lease_%d_%s", time.Now().UnixNano(), hex.EncodeToString(suffix))
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

// leaseSealer is the credential key every replica in these tests shares, derived once because
// deriving it is deliberately slow.
var leaseSealer = sync.OnceValue(func() *credential.Sealer {
	return credential.NewSealer("lease-test-passphrase", "lease-test-salt")
})

// leaseNode returns a control node's dispatcher over store, holding the credentials in creds. Two
// nodes built over one store are two replicas of one install.
func leaseNode(t *testing.T, store run.Store, creds credential.Store) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(store, roundhouse.RunnerFunc(nil), zap.NewNop(), dispatch.WithNoJanitor(),
		dispatch.WithCredentials(creds, leaseSealer()), dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	return d
}

// leaseCredentials returns a credential store holding cred_vdyn, a vault_dynamic source minting
// from v.
func leaseCredentials(t *testing.T, v *fakeVault) credential.Store {
	t.Helper()
	config, err := json.Marshal(map[string]string{"addr": v.srv.URL, "path": "database/creds/app",
		"field": "password", "token": "hvs.lease-test"})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	sealed, err := leaseSealer().Seal(string(config))
	if err != nil {
		t.Fatalf("seal config: %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{ID: "cred_vdyn", Name: "db",
		Kind: credential.KindToken, Source: secretsource.KindVaultDynamic, Secret: sealed,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	return creds
}

// leasedRun stores a relay run claimed under claim and needing cred_vdyn, and returns it.
func leasedRun(t *testing.T, store run.Store, id, claim string) *run.Run {
	t.Helper()
	r := &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusRunning,
		CredentialIDs: []string{"cred_vdyn"}, ClaimedBy: "worker-1", ClaimSecret: claim,
		CreatedAt: time.Now()}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	return r
}

// recordedLeases returns the secret lease records store holds.
func recordedLeases(t *testing.T, store run.Store) []*run.SecretLease {
	t.Helper()
	recs, err := store.(run.SecretLeases).ListSecretLeases(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListSecretLeases() error = %v", err)
	}
	return recs
}

// TestRelaySecretsAreRevokedByAnyReplicaAfterTheirClaimEnds mints a Vault dynamic secret for a
// relay run on one control node and checks what happens to it when that node is gone.
//
// A control node kept the means to revoke what it minted for a relay run only in its memory, so a
// node that stopped or restarted while the run was out forgot it, and the secret lived out its TTL.
// Now the minting node records a sealed revoke handle in the store, and any replica's sweep revokes
// the secret once the claim it was delivered under ends: the run finished, was claimed again, or is
// gone. A claim still held keeps its secret, a secret is revoked once however many replicas sweep,
// and a record past its own expiry is dropped without a revoke.
func TestRelaySecretsAreRevokedByAnyReplicaAfterTheirClaimEnds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// End ends the run's claim, or leaves it held when nil.
		End func(t *testing.T, store run.Store, r *run.Run)
		// Sweepers is how many replicas sweep at once after the claim ends.
		Sweepers int
		// Expired dates the sweep past the secret's own expiry.
		Expired bool
		// WantRevokes is how many revokes the lease receives.
		WantRevokes int
		// WantKept reports whether the record is still there afterward.
		WantKept bool
	}{{ // Test 0: The run finished, and a replica that never held the secret revokes it.
		Name: "finished", End: func(t *testing.T, store run.Store, r *run.Run) {
			r.Status = run.StatusSucceeded
			if err := store.Save(context.Background(), r); err != nil {
				t.Fatalf("save finished run: %v", err)
			}
		}, Sweepers: 1, WantRevokes: 1,
	}, { // Test 1: The run was claimed again under another lease, so the first worker is gone.
		Name: "claimed again", End: func(t *testing.T, store run.Store, r *run.Run) {
			r.ClaimSecret, r.ClaimedBy = "claim-later", "worker-2"
			if err := store.Save(context.Background(), r); err != nil {
				t.Fatalf("save reclaimed run: %v", err)
			}
		}, Sweepers: 1, WantRevokes: 1,
	}, { // Test 2: The claim is still held, so nothing is revoked and the record stays.
		Name: "still held", Sweepers: 1, WantRevokes: 0, WantKept: true,
	}, { // Test 3: Four replicas sweep together and the secret is revoked once.
		Name: "racing replicas", End: func(t *testing.T, store run.Store, r *run.Run) {
			r.Status = run.StatusFailed
			if err := store.Save(context.Background(), r); err != nil {
				t.Fatalf("save failed run: %v", err)
			}
		}, Sweepers: 4, WantRevokes: 1,
	}, { // Test 4: A secret past its own expiry is dropped without a revoke.
		Name: "expired", Sweepers: 1, Expired: true, WantRevokes: 0,
	}}
	for _, backend := range leaseBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d %s", backend.Name, testNum, test.Name), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				v := newFakeVault(t)
				store := backend.Open(t)
				creds := leaseCredentials(t, v)
				r := leasedRun(t, store, "run_lease", "claim-first")
				minter := leaseNode(t, store, creds)
				p, release, err := minter.OpenSecrets(ctx, r)
				if err != nil {
					t.Fatalf("OpenSecrets() error = %v", err)
				}
				p.Wipe()
				if release == nil {
					t.Fatal("OpenSecrets() returned no release for a minted secret")
				}
				recs := recordedLeases(t, store)
				if len(recs) != 1 {
					t.Fatalf("recorded %d handles for one minted secret, want 1", len(recs))
				}
				rec := recs[0]
				if rec.Kind != secretsource.KindVaultDynamic || rec.RunID != r.ID ||
					rec.ClaimHash != run.ClaimHash("claim-first") || rec.CredentialID != "cred_vdyn" {
					t.Errorf("record = %+v, want the run, its claim, and the credential named", rec)
				}
				if strings.Contains(rec.Handle, "lease-1") || strings.Contains(rec.Handle, "pw-1") {
					t.Errorf("the recorded handle is not sealed: %q", rec.Handle)
				}
				if test.End != nil {
					test.End(t, store, r)
				}
				// The minting node is gone: its release never runs, and the replicas sweeping hold
				// nothing in memory.
				at := time.Now()
				if test.Expired {
					at = rec.ExpiresAt.Add(time.Second)
				}
				var wg sync.WaitGroup
				for i := 0; i < test.Sweepers; i++ {
					replica := leaseNode(t, store, creds)
					wg.Add(1)
					go func() {
						defer wg.Done()
						replica.SweepSecretLeases(ctx, store.(run.SecretLeases), at)
					}()
				}
				wg.Wait()
				if diff := cmp.Diff(test.WantRevokes, v.revokes()["database/creds/app/lease-1"]); diff != "" {
					t.Errorf("revokes of the minted lease mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(test.WantKept, len(recordedLeases(t, store)) == 1); diff != "" {
					t.Errorf("record kept mismatch (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestRelaySecretReleasedByItsNodeIsRevokedOnce pins the path a live control node takes: the relay
// runs the release when the worker reports the run finished. The release takes the record and
// revokes the secret, so a sweep afterward finds nothing to revoke, and a release that finds its
// record already taken by another replica leaves the revoke to that replica.
func TestRelaySecretReleasedByItsNodeIsRevokedOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// SweepFirst lets another replica sweep the finished run before the release runs.
		SweepFirst bool
	}{{ // Test 0: The node that minted the secret releases it.
		Name: "release",
	}, { // Test 1: Another replica sweeps first, and the release does not revoke again.
		Name: "swept first", SweepFirst: true,
	}}
	for _, backend := range leaseBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d %s", backend.Name, testNum, test.Name), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				v := newFakeVault(t)
				store := backend.Open(t)
				creds := leaseCredentials(t, v)
				r := leasedRun(t, store, "run_release", "claim-first")
				minter := leaseNode(t, store, creds)
				p, release, err := minter.OpenSecrets(ctx, r)
				if err != nil || release == nil {
					t.Fatalf("OpenSecrets() release present %t, error %v, want a release for a minted "+
						"secret", release != nil, err)
				}
				p.Wipe()
				r.Status = run.StatusSucceeded
				if err := store.Save(ctx, r); err != nil {
					t.Fatalf("save finished run: %v", err)
				}
				if test.SweepFirst {
					leaseNode(t, store, creds).SweepSecretLeases(ctx, store.(run.SecretLeases), time.Now())
				}
				release()
				minter.SweepSecretLeases(ctx, store.(run.SecretLeases), time.Now())
				if diff := cmp.Diff(map[string]int{"database/creds/app/lease-1": 1}, v.revokes()); diff != "" {
					t.Errorf("revokes mismatch (-want +got):\n%s", diff)
				}
				if recs := recordedLeases(t, store); len(recs) != 0 {
					t.Errorf("%d records left after the secret was revoked, want none", len(recs))
				}
			})
		}
	}
}

// TestLocalRunSecretsAreRevokedByAnyReplicaAfterTheirProcessDies mints a Vault dynamic secret for a
// run this control node executes itself, and stops the node in the middle of the run without
// letting it clean up, the way a crash does.
//
// A run executed on a control node handed its secrets back only from that process's memory, at the
// end of the run, so a node that crashed mid-run left every secret it had minted alive until its
// TTL. The secret's handle is now recorded under the run's claim, the same as a relay run's. While
// the run executes under that claim no replica touches it. Once the janitor marks the run
// interrupted, another replica revokes the secret, before the dead node could have, and when the
// node's own cleanup does run later it finds the record taken and does not revoke a second time.
func TestLocalRunSecretsAreRevokedByAnyReplicaAfterTheirProcessDies(t *testing.T) {
	t.Parallel()
	for _, backend := range leaseBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			v := newFakeVault(t)
			store := backend.Open(t)
			keeper := store.(run.SecretLeases)
			creds := leaseCredentials(t, v)
			started, hold := make(chan struct{}), make(chan struct{})
			var once sync.Once
			// The runner stands in for a process that has stopped answering: it holds the run until
			// the test lets it go, ignoring the cancel a lost lease sends, so nothing of the node's
			// own cleanup runs while the replica sweeps.
			runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
				io.Writer) (roundhouse.Result, error) {
				once.Do(func() { close(started) })
				<-hold
				return roundhouse.Result{ExitCode: 0}, nil
			})
			node := dispatch.New(store, runner, zap.NewNop(), dispatch.WithNoJanitor(),
				dispatch.WithCredentials(creds, leaseSealer()), dispatch.WithRunFilesRoot(t.TempDir()))
			released := false
			t.Cleanup(func() {
				if !released {
					close(hold)
				}
				node.Close()
			})
			created, err := node.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand("id"), run.WithCredentialIDs([]string{"cred_vdyn"}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			select {
			case <-started:
			case <-time.After(30 * time.Second):
				t.Fatal("the run never reached its runner")
			}
			stored, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			recs := recordedLeases(t, store)
			if len(recs) != 1 || recs[0].RunID != created.ID || stored.ClaimSecret == "" ||
				recs[0].ClaimHash != run.ClaimHash(stored.ClaimSecret) {
				t.Fatalf("records = %+v, want one bound to the run's claim", recs)
			}
			replica := leaseNode(t, store, creds)
			replica.SweepSecretLeases(ctx, keeper, time.Now())
			if diff := cmp.Diff(0, v.revokes()["database/creds/app/lease-1"]); diff != "" {
				t.Fatalf("a replica revoked the secret of a run still executing (-want +got):\n%s",
					diff)
			}
			// The node is gone: its lease goes unrenewed and the janitor marks the run interrupted.
			// The node's watcher can still renew the lease in the instant a reclaim reads it, so the
			// reclaim is repeated until the run is interrupted rather than assumed to take at once.
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := store.ReclaimStale(ctx, 0); err != nil {
					t.Fatalf("ReclaimStale() error = %v", err)
				}
				if got, gerr := store.Get(ctx, created.ID); gerr == nil && got.Status.Terminal() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the run was never marked interrupted")
				}
				time.Sleep(10 * time.Millisecond)
			}
			replica.SweepSecretLeases(ctx, keeper, time.Now())
			if diff := cmp.Diff(1, v.revokes()["database/creds/app/lease-1"]); diff != "" {
				t.Errorf("revokes after the run was interrupted mismatch (-want +got):\n%s", diff)
			}
			released = true
			close(hold)
			node.Close()
			if diff := cmp.Diff(map[string]int{"database/creds/app/lease-1": 1}, v.revokes()); diff != "" {
				t.Errorf("revokes after the node's own cleanup ran mismatch (-want +got):\n%s", diff)
			}
			if recs := recordedLeases(t, store); len(recs) != 0 {
				t.Errorf("%d records left after the secret was revoked, want none", len(recs))
			}
		})
	}
}

// TestASecretOpenedOutsideAClaimIsNeverRecorded opens a run's dynamic secret the way the plan gate
// does when it plans a run at submission: for a run nobody has claimed, which may not be stored
// yet.
//
// A recorded handle is revoked by any replica whose sweep finds its claim over, and a run with no
// claim, or no row, reads as over. Recording this secret would let a sweep revoke it while the scan
// was still using it. It is held for the one bounded scan instead and revoked when the scan ends.
func TestASecretOpenedOutsideAClaimIsNeverRecorded(t *testing.T) {
	t.Parallel()
	for _, backend := range leaseBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			v := newFakeVault(t)
			store := backend.Open(t)
			creds := leaseCredentials(t, v)
			scanner := leaseNode(t, store, creds)
			unclaimed := &run.Run{ID: "run_gate", Tool: run.ToolBash, Command: "id",
				CredentialIDs: []string{"cred_vdyn"}}
			cleanup, err := scanner.MaterializeCredentials(ctx, unclaimed, &roundhouse.Spec{})
			if err != nil {
				t.Fatalf("MaterializeCredentials() error = %v", err)
			}
			if recs := recordedLeases(t, store); len(recs) != 0 {
				t.Errorf("recorded %d handles for a secret opened outside any claim, want none",
					len(recs))
			}
			leaseNode(t, store, creds).SweepSecretLeases(ctx, store.(run.SecretLeases), time.Now())
			if diff := cmp.Diff(0, v.revokes()["database/creds/app/lease-1"]); diff != "" {
				t.Errorf("a sweep revoked a secret still in use by a scan (-want +got):\n%s", diff)
			}
			cleanup()
			if diff := cmp.Diff(1, v.revokes()["database/creds/app/lease-1"]); diff != "" {
				t.Errorf("revokes once the scan ended mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
