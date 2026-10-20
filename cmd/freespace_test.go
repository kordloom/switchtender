package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
)

// TestFreeSpaceFloorWatchesTheDatabase pins what serve's floor watches: a SQLite database's
// directory and nothing else, nothing for PostgreSQL or 0, a usage error for a negative value, and
// no floor when the directory's space cannot be read at startup.
func TestFreeSpaceFloorWatchesTheDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unreadable := errors.New("statfs: not supported")
	tests := []struct {
		DB        string
		MinMiB    int
		ReadErr   error
		WantFloor uint64
		WantPaths []string
		WantReads int
		Want      error
	}{{ // Test 0: A SQLite database's directory is watched at the given floor.
		DB: filepath.Join(dir, "switchtender.db"), MinMiB: 256, WantFloor: 256 << 20,
		WantPaths: []string{dir}, WantReads: 1,
	}, { // Test 1: 0 turns it off without reading.
		DB: filepath.Join(dir, "switchtender.db"), MinMiB: 0,
	}, { // Test 2: A PostgreSQL server has no floor.
		DB: "postgres://st@db.internal/st", MinMiB: 256,
	}, { // Test 3: A negative value is a usage error.
		DB: filepath.Join(dir, "switchtender.db"), MinMiB: -1, Want: ErrUsage,
	}, { // Test 4: A directory whose space cannot be read turns the floor off.
		DB: filepath.Join(dir, "switchtender.db"), MinMiB: 256, ReadErr: unreadable, WantReads: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			reads := 0
			free := func(string) (uint64, error) {
				reads++
				return 1 << 40, test.ReadErr
			}
			floor, paths, err := freeSpaceFloor(test.DB, test.MinMiB, free, zap.NewNop())
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if floor != test.WantFloor {
				t.Errorf("floor = %d, want %d", floor, test.WantFloor)
			}
			if diff := cmp.Diff(test.WantPaths, paths, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("paths mismatch (-want +got):\n%s", diff)
			}
			if reads != test.WantReads {
				t.Errorf("reads = %d, want %d", reads, test.WantReads)
			}
		})
	}
}
