package dispatch

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// skipWebhookEvent is the event a webhook payload names for a skipped schedule fire.
const skipWebhookEvent = "schedule.skipped"

// skipReasonFallback is the reason a skipped fire's notice states when the notice carries none.
const skipReasonFallback = "no hosts matched"

// AnnounceSkip tells the named notification targets that a schedule's fire was skipped because its
// inventory matched no hosts. r is the run-shaped notice the scheduler builds, which is never
// stored. It takes the same named-target path a run takes, so a target attached to the schedule for
// the skipped event, or for failure, hears it once. The server-wide channels and a template's
// inline targets report runs and holds, and a skip is neither, so they never hear one.
//
// The notice names the schedule. The inventory's name is added here, where the inventory store is,
// so the message says which inventory matched nothing rather than only its id.
func (d *Dispatcher) AnnounceSkip(r *run.Run) {
	if r == nil || r.Kind != run.KindSkippedFire {
		return
	}
	snap := r.Clone()
	if snap.InventoryID != "" && d.inventories != nil {
		if inv, err := d.inventories.Get(context.Background(), snap.InventoryID); err == nil {
			labels := maps.Clone(snap.Labels)
			if labels == nil {
				labels = map[string]string{}
			}
			labels["inventory"] = inv.Name
			snap.Labels = labels
		}
	}
	d.notifyNamed(snap)
}

// isSkip reports whether r is the notice of a skipped schedule fire rather than a run.
func isSkip(r *run.Run) bool {
	return r != nil && r.Kind == run.KindSkippedFire
}

// skipSchedule names the schedule a skipped fire belongs to, falling back to its id.
func skipSchedule(r *run.Run) string {
	if name := r.Labels["schedule"]; name != "" {
		return name
	}
	return r.SourceID
}

// skipInventory names the inventory that matched no hosts, its name when known and else its id.
func skipInventory(r *run.Run) string {
	if name := r.Labels["inventory"]; name != "" {
		return name
	}
	return r.InventoryID
}

// skipReason is why the fire started nothing, as the notice states it.
func skipReason(r *run.Run) string {
	if r.Warning != "" {
		return r.Warning
	}
	return skipReasonFallback
}

// skipHeadline is the first line every channel shows for a skipped fire, after its own rendering
// of the schedule's name: that it was skipped and why. It never says failed, because nothing did.
func skipHeadline(r *run.Run) string {
	return "skipped: " + skipReason(r) + "."
}

// skipDetail says what the skip means for whoever reads it: which inventory resolved to nothing,
// and that no run was started.
func skipDetail(r *run.Run) string {
	msg := "No run was started."
	if inv := skipInventory(r); inv != "" {
		msg = "Its inventory " + strconv.Quote(inv) + " resolved to no hosts, so no run was started."
	}
	return msg
}

// skipTeamsFacts is the fact set a Teams card shows for a skipped fire.
func skipTeamsFacts(r *run.Run) []map[string]string {
	facts := []map[string]string{
		{"title": "Status", "value": "skipped"},
		{"title": "Reason", "value": skipReason(r)},
	}
	if inv := skipInventory(r); inv != "" {
		facts = append(facts, map[string]string{"title": "Inventory", "value": inv})
	}
	return append(facts, map[string]string{"title": "Run", "value": "none started"})
}

// skipEmailBody renders the plain-text email for a skipped fire: what was skipped and why, what it
// was set to reach, and where to see what the inventory matches now.
func skipEmailBody(r *run.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Schedule %s %s\n\n", strconv.Quote(skipSchedule(r)), skipHeadline(r))
	fmt.Fprintf(&b, "Schedule: %s\n", r.SourceID)
	if inv := skipInventory(r); inv != "" {
		fmt.Fprintf(&b, "Inventory: %s\n", inv)
	}
	if !r.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "Fired at: %s\n", r.CreatedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\nNo run was started. The schedule keeps firing on its cadence and starts a run " +
		"as soon as its inventory matches a host.\n")
	if r.InventoryID != "" {
		fmt.Fprintf(&b, "See which hosts the inventory matches now: /ui/inventories?preview=%s\n",
			r.InventoryID)
	}
	return b.String()
}
