package pgstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestIdempotencyKeysWithASeparatorByteOnPostgreSQL stores and looks up the two idempotency keys
// the server derives with a NUL byte inside them: a provisioning callback's replay key, which joins
// the template id and the host, and a client's Idempotency-Key on an install with organizations,
// which run.ClientKey scopes to the organization with the same byte.
//
// PostgreSQL refuses a NUL in a text value with SQLSTATE 22021, so neither key can be looked up or
// saved: the lookup returns an error rather than run.ErrNotFound, and every submit carrying one
// fails. SQLite stores the byte, so the SQLite contract and the SQLite-only callback scenario pass
// while every provisioning callback, and every organization's retried submit, fails on the backend
// high availability runs on. The store, or the key derivation, has to carry these keys intact.
func TestIdempotencyKeysWithASeparatorByteOnPostgreSQL(t *testing.T) {
	t.Parallel()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	db, err := Open(freshDatabase(t, shared))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	orgKey, err := run.ClientKey("deploy-2026-10-04", "org_a")
	if err != nil {
		t.Fatalf("ClientKey() error = %v", err)
	}
	tests := []struct {
		Name string
		Key  string
	}{{ // Test 0: The key a provisioning callback derives for its replay guard.
		Name: "callback replay key",
		Key:  run.DedupeKey("callback", "tpl_cb\x00web01", time.Now()),
	}, { // Test 1: A client's key scoped to its organization.
		Name: "organization client key",
		Key:  orgKey,
	}}
	ctx := context.Background()
	for testNum, test := range tests {
		if _, err := db.Runs().ByIdempotencyKey(ctx, test.Key); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("test %d %s: ByIdempotencyKey() on an unused key error = %v, want "+
				"run.ErrNotFound", testNum, test.Name, err)
		}
		r := &run.Run{ID: "run_nul_" + string(rune('a'+testNum)), Playbook: "site.yml",
			Status: run.StatusPending, IdempotencyKey: test.Key, CreatedAt: time.Now()}
		if err := db.Runs().Save(ctx, r); err != nil {
			t.Errorf("test %d %s: Save() of a run carrying the key error = %v", testNum, test.Name,
				err)
			continue
		}
		got, err := db.Runs().ByIdempotencyKey(ctx, test.Key)
		if err != nil || got.ID != r.ID {
			t.Errorf("test %d %s: ByIdempotencyKey() after the save = %v, %v, want %s", testNum,
				test.Name, got, err, r.ID)
		}
	}
}
