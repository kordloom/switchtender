package notification

import "time"

// Target states, as a target's delivery status reports them.
const (
	// StateNeedsSecret is a target imported without its secret, which delivers nothing until the
	// secret is entered. It is neither configured nor healthy.
	StateNeedsSecret = "needs_secret"
	// StateConfigured is a complete target that has not delivered anything yet.
	StateConfigured = "configured"
	// StateHealthy is a target whose most recent finished delivery reached it.
	StateHealthy = "healthy"
	// StateRetrying is a target with a delivery being retried after a failed attempt.
	StateRetrying = "retrying"
	// StateFailing is a target whose most recent finished delivery failed for good.
	StateFailing = "failing"
)

// HealthWindow is how far back a target's delivery status counts failures, and how long a failed
// delivery keeps a target listed by the doctor.
const HealthWindow = 7 * 24 * time.Hour

// Health is a target's delivery status: the state it is in and what its recent deliveries came to.
type Health struct {
	// State is needs_secret, configured, healthy, retrying, or failing.
	State string `json:"state"`
	// Missing names the secret parts a target waiting for its secret lacks: url, key, or both.
	Missing []string `json:"missing,omitempty"`
	// LastDeliveredAt is when a delivery last reached the target, the latest finish among them.
	LastDeliveredAt *time.Time `json:"last_delivered_at,omitempty"`
	// LastFailedAt is when a delivery to the target last failed for good, the latest finish among
	// them.
	LastFailedAt *time.Time `json:"last_failed_at,omitempty"`
	// LastError is why that delivery failed.
	LastError string `json:"last_error,omitempty"`
	// Failed counts the deliveries that failed for good within the window, by when they failed.
	Failed int `json:"failed"`
	// Pending counts the deliveries still waiting, for their turn or their next attempt.
	Pending int `json:"pending"`
}

// HealthOf summarizes a target's delivery status from its recent deliveries. Which delivery is the
// latest is decided by when each one finished, never by the order they were recorded in: a delivery
// retried for minutes finishes after the ones recorded behind it, and its outcome is the newer news
// about the target. Failures that finished before since are not counted. A target waiting for its
// secret is in that state whatever its history says, since it delivers nothing until the secret is
// entered.
func HealthOf(n *Notification, recent []*Delivery, since time.Time) Health {
	h := Health{State: StateConfigured, Missing: n.Missing()}
	var delivered, failed *Delivery
	retrying := false
	for _, d := range recent {
		switch d.Status {
		case DeliveryPending:
			h.Pending++
			if d.Attempts > 0 {
				retrying = true
			}
		case DeliveryDelivered:
			delivered = finishedLater(delivered, d)
		case DeliveryFailed:
			failed = finishedLater(failed, d)
			if !finishedAt(d).Before(since) {
				h.Failed++
			}
		}
	}
	if delivered != nil {
		h.LastDeliveredAt = delivered.FinishedAt
	}
	if failed != nil {
		h.LastFailedAt, h.LastError = failed.FinishedAt, failed.LastError
	}
	switch {
	case n.NeedsSecret:
		h.State = StateNeedsSecret
	case failed != nil && (delivered == nil || !finishedAt(delivered).After(finishedAt(failed))):
		h.State = StateFailing
	case retrying:
		h.State = StateRetrying
	case delivered != nil:
		h.State = StateHealthy
	}
	return h
}

// finishedLater returns whichever of two deliveries finished later, keeping a when they finished at
// the same instant. Either may be nil.
func finishedLater(a, b *Delivery) *Delivery {
	if a == nil || (b != nil && finishedAt(b).After(finishedAt(a))) {
		return b
	}
	return a
}

// finishedAt returns when a delivery finished, or when it was recorded for one with no finish time.
func finishedAt(d *Delivery) time.Time {
	if d.FinishedAt != nil {
		return *d.FinishedAt
	}
	return d.CreatedAt
}
