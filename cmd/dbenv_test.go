package cmd

import (
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

// TestDBFromEnv pins where serve and worker take their database from. A PostgreSQL DSN carries its
// password, and passed as --db it showed in the process list and in every chart install's pod spec,
// so the environment names it when the flag is absent, and the flag still wins when it is given.
func TestDBFromEnv(t *testing.T) {
	tests := []struct {
		WantResult string
		Env        string
		Flag       string
	}{{ // Test 0: Neither: the default stands.
		WantResult: defaultDBPath,
	}, { // Test 1: The environment names it.
		Env: "postgres://st:secret@db/st", WantResult: "postgres://st:secret@db/st",
	}, { // Test 2: The flag, given, wins over the environment.
		Env: "postgres://st:secret@db/st", Flag: "/data/switchtender.db", WantResult: "/data/switchtender.db",
	}, { // Test 3: Whitespace is not a database.
		Env: "  ", WantResult: defaultDBPath,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv.
			t.Setenv(dbEnvVar, test.Env)
			cmd := &cobra.Command{}
			var db string
			cmd.Flags().StringVar(&db, "db", defaultDBPath, "")
			if test.Flag != "" {
				if err := cmd.Flags().Set("db", test.Flag); err != nil {
					t.Fatalf("Set() error = %v", err)
				}
			}
			if got := dbFromEnv(cmd, db); got != test.WantResult {
				t.Errorf("dbFromEnv() = %q, want %q", got, test.WantResult)
			}
		})
	}
}
