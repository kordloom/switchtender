package cmd

import (
	"fmt"
	"path/filepath"

	"go.uber.org/zap"
)

// defaultMinFreeMiB is the free space, in MiB, below which serve refuses new runs. It is room for
// each of the default four workers to reach the 64 MiB log cap, and on the chart's 2 Gi volume it
// trips at seven eighths full.
const defaultMinFreeMiB = 256

// serveMinFreeMiB holds the value of the --min-free-mib flag.
var serveMinFreeMiB int

// freeSpaceFloor returns the floor, in bytes, and the directories it watches for a server on db.
//
// Only a SQLite database's directory is watched. Its disk is the one whose filling loses a run's
// output and leaves its end unrecorded, while a full run files root already fails a run cleanly
// when it cannot create the run's directory, and is small on purpose. A PostgreSQL server's disk is
// not this process's to measure. A directory whose free space cannot be read at startup turns the
// floor off with a warning, since a filesystem that cannot report it would otherwise read as
// degraded forever.
func freeSpaceFloor(db string, minMiB int, free func(string) (uint64, error), log *zap.Logger) (uint64, []string, error) {
	if minMiB < 0 {
		return 0, nil, fmt.Errorf("%w: --min-free-mib must be 0 or more, got %d", ErrUsage, minMiB)
	}
	if minMiB == 0 || isPostgresDSN(db) {
		return 0, nil, nil
	}
	dir, err := filepath.Abs(filepath.Dir(db))
	if err != nil {
		return 0, nil, fmt.Errorf("free space floor: database directory: %w", err)
	}
	if _, err := free(dir); err != nil {
		log.Warn("serve: free space floor off: " + err.Error())
		return 0, nil, nil
	}
	floor := uint64(minMiB) << 20
	log.Info("serve: new runs are refused while the database's disk is under the free space floor",
		zap.String("path", dir), zap.Int("min_free_mib", minMiB))
	return floor, []string{dir}, nil
}
