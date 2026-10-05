package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// nulHostInventory names its one host with a NUL byte, which a YAML double-quoted escape spells.
const nulHostInventory = "all:\n  hosts:\n    \"web\\0one\":\n      ansible_host: 10.0.0.11\n"

// TestCallbackForAHostNamedWithANulByteOnPostgreSQL calls back for a host whose inventory name
// carries a NUL byte, on a server whose runs and audit chain are both PostgreSQL.
//
// The host's name reaches three places a text column holds: the run's limit, the dedupe key, and
// the actor the chain entry records before anything launches. PostgreSQL refuses a NUL in any of
// them, and the chain entry is fail-closed, so the callback answered 503 and the host was never
// provisioned. The repeated callback must then find the first run, which is stored under the
// cleaned name, and answer 409 naming it.
func TestCallbackForAHostNamedWithANulByteOnPostgreSQL(t *testing.T) {
	t.Parallel()
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
	ctx := context.Background()
	if err := db.Inventories().Save(ctx, &inventory.Inventory{ID: "inv_nul", Name: "nul",
		Content: nulHostInventory, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := db.Templates().Save(ctx, &template.Template{ID: "tpl_cb", Name: "boot",
		Playbook: "boot.yml", InventoryID: "inv_nul", AllowCallbacks: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(db.Runs(), runner, zap.NewNop(), dispatch.WithInventories(db.Inventories()),
		dispatch.WithNoJanitor(), dispatch.WithQueues([]string{"cbk-unserved"}),
		dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	h := New(db.Runs(), d, zap.NewNop(), WithTemplates(db.Templates()),
		WithInventories(db.Inventories()), WithAudit(db.Audits()),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(srv *Server) { srv.callbackResolver = fakeResolver{} }).Handler()
	key := (&cbkReplica{handler: h}).mint(t)
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, cbkNative, strings.NewReader(keyBody(key)))
		req.RemoteAddr = "10.0.0.11:40000"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := call()
	if first.Code != http.StatusCreated {
		t.Fatalf("a callback for a host named with a NUL byte = %d, want 201: %s", first.Code,
			strings.TrimSpace(first.Body.String()))
	}
	again := call()
	if again.Code != http.StatusConflict || again.Header().Get("Location") !=
		first.Header().Get("Location") {
		t.Errorf("a repeated callback = %d at %q, want 409 naming the first run %q: %s", again.Code,
			again.Header().Get("Location"), first.Header().Get("Location"),
			strings.TrimSpace(again.Body.String()))
	}
}

// TestPendingCallbackRunFindsAHostStoredCleaned pins the pending check for a host whose name a text
// column cannot hold. Every store cleans a run's limit before it writes, so the check has to compare
// the cleaned name: comparing the raw one never found the run, and a second callback for the host
// was refused only while the ten-second dedupe key still matched.
func TestPendingCallbackRunFindsAHostStoredCleaned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	tests := []struct {
		Host string
	}{{ // Test 0: A NUL byte, which a YAML escape puts in a host name.
		Host: "web\x00one",
	}, { // Test 1: A byte that is not UTF-8.
		Host: "web\xffone",
	}, { // Test 2: An ordinary name.
		Host: "web01",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			if err := store.Save(ctx, &run.Run{ID: id, Playbook: "boot.yml",
				Status: run.StatusPending, Source: run.SourceCallback, SourceID: "tpl_" + id,
				Limit: test.Host, CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			got, err := pendingCallbackRun(ctx, store, "tpl_"+id, test.Host)
			if err != nil || got == nil || got.ID != id {
				t.Errorf("pendingCallbackRun(%q) = %v, %v, want %s", test.Host, got, err, id)
			}
		})
	}
}
