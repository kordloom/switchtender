package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/run"
)

// TestOpenUpgradesKeysAnEarlierReleaseStored covers an install upgrading in place. Earlier releases
// stored a key scoped to an organization and a provisioning callback's replay key with a NUL byte
// inside, and a caller's key that was not UTF-8 as it arrived, and SQLite kept them. This release
// derives text in their place, so the open has to rewrite each stored key to the one derived now:
// otherwise an organization's retried submission misses the run it already made and fires a second,
// and a backup of the install could never be restored onto PostgreSQL.
func TestOpenUpgradesKeysAnEarlierReleaseStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	bucket := strconv.FormatInt(at.UnixNano()/int64(run.DedupeWindow), 10)
	scoped, err := run.ClientKey("deploy-1", "org_a")
	if err != nil {
		t.Fatalf("ClientKey() error = %v", err)
	}
	unscoped, err := run.ClientKey("deploy\xff", "")
	if err != nil {
		t.Fatalf("ClientKey() error = %v", err)
	}
	tests := []struct {
		Name    string
		RunID   string
		Stored  string
		WantKey string
	}{{ // Test 0: A key scoped to an organization.
		Name: "scoped", RunID: "run_scoped", Stored: "org_a\x00deploy-1", WantKey: scoped,
	}, { // Test 1: A provisioning callback's replay key.
		Name: "callback", RunID: "run_callback", Stored: "st:callback:tpl_cb\x00web01:" + bucket,
		WantKey: run.DedupeKey("callback", "tpl_cb\x00web01", at),
	}, { // Test 2: A caller's key that was not UTF-8.
		Name: "not utf8", RunID: "run_raw", Stored: "deploy\xff", WantKey: unscoped,
	}, { // Test 3: A key that was already text keeps its shape.
		Name: "plain", RunID: "run_plain", Stored: "nightly", WantKey: "nightly",
	}}
	path := filepath.Join(t.TempDir(), "st.db")
	earlier, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, test := range tests {
		r := &run.Run{ID: test.RunID, Playbook: "site.yml", Status: run.StatusSucceeded,
			IdempotencyKey: test.Stored, CreatedAt: at}
		if err := earlier.Runs().Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", test.RunID, err)
		}
	}
	if err := earlier.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			got, err := upgraded.Runs().ByIdempotencyKey(ctx, test.WantKey)
			if err != nil || got.ID != test.RunID {
				t.Errorf("ByIdempotencyKey(the key derived now) = %v, %v, want %s", got, err,
					test.RunID)
			}
		})
	}
	rows, err := upgraded.db.w.QueryContext(ctx, "SELECT idempotency_key FROM runs")
	if err != nil {
		t.Fatalf("read stored keys: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan a stored key: %v", err)
		}
		if !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			t.Errorf("the reopened database still holds the key %q, which PostgreSQL refuses", key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stored keys: %v", err)
	}
}
