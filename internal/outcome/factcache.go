package outcome

import "github.com/kordloom/switchtender/internal/run"

// FactCacheSpec is the fact cache setting a spec record binds.
//
// A cached fact stands in for one the play would have gathered, so a play that branches on a fact
// can act on a host as it was when the facts were cached rather than as it is now. That makes the
// setting part of what an approver decides on, and part of what the executor holds an approved run
// to.
type FactCacheSpec struct {
	// TimeoutSeconds is how many seconds cached facts stay fresh enough to serve. Zero serves them
	// however old they are, and is written out rather than omitted so the record states it.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// factCacheSpecOf returns the fact cache setting r's spec record binds, or nil when r does not use
// the cache. A timeout left on a run with the cache off executes nothing, so it must not move the
// digest either, and nil keeps the record of every such run exactly what it was before the setting
// was bound.
func factCacheSpecOf(r *run.Run) *FactCacheSpec {
	if !r.UseFactCache {
		return nil
	}
	return &FactCacheSpec{TimeoutSeconds: r.FactCacheTimeout}
}
