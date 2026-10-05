package pgstore

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/storetest"
)

// TestCallbackGuardsCrossReplicas opens two pool handles on one database, the way two server
// replicas open one shared PostgreSQL, and proves the callback lane and the budgets are held by the
// database rather than by either handle.
func TestCallbackGuardsCrossReplicas(t *testing.T) {
	t.Parallel()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	a, err := Open(dsn)
	if err != nil {
		t.Fatalf("open handle a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("open handle b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	storetest.CallbackGuardsCrossReplicas(t, a.Runs(), b.Runs())
}
