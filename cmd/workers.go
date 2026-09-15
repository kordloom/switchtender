package cmd

import (
	"fmt"
)

// checkWorkers refuses a --workers value that cannot mean what it says.
//
// The flag reads "concurrent runs this process executes at once", so zero looks like the way to
// stand up a node that serves the API and executes nothing, which is a reasonable thing to want:
// it keeps run credentials off the process holding the public listener. It never did that. The
// dispatcher treats a pool size below one as unset and substitutes the default, so --workers 0
// started four of them and the operator had no way to tell from the outside. Whoever set it for
// that reason got the opposite of it, silently, on the node they were trying to keep clean.
//
// Refusing is the honest answer while the server executes its own runs. Dedicated workers are
// additive rather than a way to move execution off the server, so there is nothing to point a
// zero at yet.
func checkWorkers(n int, hint string) error {
	if n >= 1 {
		return nil
	}
	return fmt.Errorf("%w: --workers must be at least 1, got %d. %s", ErrUsage, n, hint)
}

// The two hints differ because the two commands are asked for zero for different reasons. Somebody
// setting it on the server wants a node that executes nothing; somebody setting it on a worker has
// typed a value that leaves the process with no reason to exist.
const (
	// serveWorkersHint answers the operator trying to keep execution off the API node.
	serveWorkersHint = "The server executes its own runs, so there is no setting that makes it " +
		"execute none. Dedicated workers add capacity alongside it rather than taking execution off it."
	// workerWorkersHint answers a worker asked to execute nothing, which would idle forever.
	workerWorkersHint = "A worker with no slots leases nothing and would sit idle. Stop the worker " +
		"instead, or give it the number of concurrent runs it should take."
)
