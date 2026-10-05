package dispatch

import "sync"

// stagedFiles removes what one execution staged under the run-files root, each piece once,
// whichever reaches it first: the call made just before the run's terminal record, or the deferred
// backstop that covers a panic.
//
// The run-files and secrets pages promise a run's directories are gone before the run is recorded
// as finished, on every way a run ends. Only the credential directory kept that order. The stored
// inventory's directory and the fact cache's were removed by deferred calls that ran after the
// terminal record was written, so a process killed in between left a finished run's secret host
// variables and cached facts on disk, for the sweep to find minutes later, or for good on a host
// whose last process had just died.
type stagedFiles struct {
	// mu guards removers.
	mu sync.Mutex
	// removers are the removals not yet run, in the order they were staged.
	removers []func()
}

// add stages one removal. A nil removal is ignored.
func (s *stagedFiles) add(remove func()) {
	if remove == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removers = append(s.removers, remove)
}

// removeAll runs every staged removal not yet run, the most recently staged first, and forgets
// them, so a second call does nothing.
func (s *stagedFiles) removeAll() {
	s.mu.Lock()
	removers := s.removers
	s.removers = nil
	s.mu.Unlock()
	for i := len(removers) - 1; i >= 0; i-- {
		removers[i]()
	}
}
