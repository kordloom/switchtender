package cmd

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestWorkerSourceRedactsDSNPassword proves the line the worker logs at startup names where it
// leases runs from without carrying the database password with it. The value is written to the log
// on every start, so an unredacted DSN puts the credential in whatever aggregates those logs.
func TestWorkerSourceRedactsDSNPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the worker configuration under test.
		Name string
		// DB is the --db value the worker was started with.
		DB string
		// Server is the --server value, non-empty for a relay worker.
		Server string
		// WantSource is the exact text the startup log must carry.
		WantSource string
	}{{ // Test 0: A DSN with credentials is logged with them removed.
		Name:       "dsn with password",
		DB:         "postgres://switchtender:s3cr3tdbpassword@db.internal:5432/switchtender",
		WantSource: "postgres://***@db.internal:5432/switchtender",
	}, { // Test 1: A local database file has no credentials and is logged whole.
		Name:       "sqlite path",
		DB:         "/var/lib/switchtender/switchtender.db",
		WantSource: "/var/lib/switchtender/switchtender.db",
	}, { // Test 2: A relay worker leases over http, so the control node URL is the source.
		Name:       "relay server",
		DB:         "postgres://switchtender:s3cr3tdbpassword@db.internal:5432/switchtender",
		Server:     "https://switchtender.internal",
		WantSource: "https://switchtender.internal",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			// Not parallel: the worker command reads package-level flag variables.
			workerDB, workerServer = test.DB, test.Server
			t.Cleanup(func() { workerDB, workerServer = "", "" })

			if diff := cmp.Diff(test.WantSource, workerSource()); diff != "" {
				t.Errorf("workerSource() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
