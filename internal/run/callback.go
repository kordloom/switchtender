package run

// SourceCallback is the source a provisioning callback stamps on the run it launches, so the runs
// view, the pending check, the stores' guard, and an auditor all name the same thing.
const SourceCallback = "callback"

// LiveCallback reports whether r is a provisioning callback run that has not finished. Every store
// holds at most one such run per template and host, the template in SourceID and the host in Limit,
// and refuses a second with ErrCallbackPending.
//
// The guard is the store's because the race it closes runs across processes. A callback checks for
// an unfinished run, records itself on the chain, and only then saves its run, so two replicas
// answering one host's callbacks at once both found nothing pending, and the dedupe key they also
// carry is a ten-second wall-clock bucket that two requests either side of a boundary do not share.
// Both launched, which ran a provisioning playbook against one host twice at once.
func LiveCallback(r *Run) bool {
	return r != nil && r.Source == SourceCallback && r.ParentID == nil && !r.Status.Terminal()
}

// sameCallbackLane reports whether a and b are callback runs for one template and one host.
func sameCallbackLane(a, b *Run) bool {
	return a.SourceID == b.SourceID && a.Limit == b.Limit
}
