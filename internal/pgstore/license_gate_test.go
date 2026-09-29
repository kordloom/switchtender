package pgstore

import (
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestOpenRefusesANewSchemaOnCommunity covers the one licensed act in this package: initializing a
// schema that does not exist yet. It needs a real server, so it runs exactly when the rest of the
// suite does. Not parallel: it swaps the process license out and back.
func TestOpenRefusesANewSchemaOnCommunity(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	team := license.Current()
	license.Set(nil)
	defer license.Set(team)

	// The DSN's database already holds a schema from the suite around this test, so the refusal
	// has to be proven against a database that does not: point at one that was never initialized.
	fresh := strings.Replace(dsn, "/switchtender?", "/postgres?", 1)
	if fresh == dsn {
		t.Skipf("cannot derive a fresh database from %q", dsn)
	}
	db, err := Open(fresh)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() initialized a new schema with no license")
	}
	if !strings.Contains(err.Error(), "Team license") {
		t.Errorf("refusal does not name the tier: %v", err)
	}
}
