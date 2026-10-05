package audit

import "strings"

// outcomeRunsPrefix and outcomeMarker frame an outcome entry's path, which reads
// /runs/<run id>/outcome/<status>.
const (
	outcomeRunsPrefix = "/runs/"
	outcomeMarker     = "/outcome/"
)

// OutcomePath returns the path an outcome entry for runID carries when the run ended in status.
func OutcomePath(runID, status string) string {
	return outcomeRunsPrefix + runID + outcomeMarker + status
}

// OutcomeRunID returns the run whose outcome e records, and false when e is not a run's outcome
// entry. A store reads it to keep a run to one outcome entry. See ErrOutcomeRecorded.
func OutcomeRunID(e *Entry) (string, bool) {
	if e == nil || e.Method != MethodRun {
		return "", false
	}
	rest, ok := strings.CutPrefix(e.Path, outcomeRunsPrefix)
	if !ok {
		return "", false
	}
	id, status, ok := strings.Cut(rest, outcomeMarker)
	if !ok || id == "" || status == "" || strings.Contains(status, "/") {
		return "", false
	}
	return id, true
}
