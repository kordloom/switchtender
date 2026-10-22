package dispatch

import (
	"fmt"

	"github.com/kordloom/switchtender/internal/runfiles"
)

// spaceFloor refuses new runs while a watched directory's filesystem has less free space than min.
// The zero value watches nothing and refuses nothing.
type spaceFloor struct {
	// min is the free bytes below which new runs are refused.
	min uint64
	// paths are the directories whose filesystems are watched.
	paths []string
	// free reads the bytes free on the filesystem a path is on.
	free func(path string) (uint64, error)
}

// newSpaceFloor returns a floor of minBytes over paths, read through free, or through
// runfiles.FreeBytes when free is nil.
func newSpaceFloor(minBytes uint64, paths []string, free func(string) (uint64, error)) spaceFloor {
	if free == nil {
		free = runfiles.FreeBytes
	}
	return spaceFloor{min: minBytes, paths: paths, free: free}
}

// shortfalls returns one line for each watched path under the floor, and the read errors apart,
// since a read that fails says nothing about the space.
func (f spaceFloor) shortfalls() (short []string, errs []error) {
	if f.min == 0 {
		return nil, nil
	}
	for _, p := range f.paths {
		free, err := f.free(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if free < f.min {
			short = append(short, fmt.Sprintf("only %s free at %s, under the %s floor",
				mib(free), p, mib(f.min)))
		}
	}
	return short, errs
}

// admit refuses a new run with ErrLowSpace while a watched path is under the floor. A path whose
// space could not be read admits, and the error is logged: a transient read failure is not taken
// for a full disk.
func (d *Dispatcher) admit() error {
	short, errs := d.floor.shortfalls()
	for _, err := range errs {
		d.log.Warn("dispatch: free space floor: admitting the run unchecked: " + err.Error())
	}
	if len(short) > 0 {
		return fmt.Errorf("%w: %s", ErrLowSpace, short[0])
	}
	return nil
}

// Admit refuses a new run with ErrLowSpace while a watched path is under the floor, the check every
// submit path here makes. It is exported for a caller that stores a run without this dispatcher,
// such as the relay proposing a worker's apply, so that run faces the same floor.
func (d *Dispatcher) Admit() error {
	return d.admit()
}

// mib renders a byte count in whole mebibytes, rounded down.
func mib(n uint64) string {
	return fmt.Sprintf("%d MiB", n>>20)
}
