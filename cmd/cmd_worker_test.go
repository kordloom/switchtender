package cmd

import (
	"fmt"
	"strings"
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

// TestWorkerReadsPoliciesFromTheSameFileTheControlNodeDoes pins that a worker can be pointed at the
// policy file, so the plan-content gate does not depend on which process claims the run.
//
// The gate is enforced wherever the run executes, and a worker read the policies only from the
// database. An install that pins its policies to a file leaves that table empty, so a worker that won
// the claim race applied a destroy with no plan, no hold, and no error, while the identical run was
// held whenever the control node claimed it. Enforcement of the product's central control was a coin
// flip decided by which process happened to be free, contradicting the two-process topology the
// reliability documentation describes.
func TestWorkerReadsPoliciesFromTheSameFileTheControlNodeDoes(t *testing.T) {
	flag := workerCmd.Flags().Lookup("policy-file")
	if flag == nil {
		t.Fatal("worker has no --policy-file, so a file-pinned install cannot give it the rules and " +
			"the gate applies only when the control node claims the run")
	}
	if !strings.Contains(flag.Usage, "policies") {
		t.Errorf("--policy-file usage does not say what it is for: %q", flag.Usage)
	}
	// The control node's own flag is the thing it has to agree with.
	if serveCmd.Flags().Lookup("policy-file") == nil {
		t.Fatal("serve has no --policy-file, so this test is pinned to the wrong name")
	}
}
