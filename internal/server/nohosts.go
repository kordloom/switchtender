package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// noHostsResponse is the refusal a manual launch gets when its composed inventory resolves to no
// hosts: the reason, and where to see what the inventory matches now.
type noHostsResponse struct {
	// Error is the reason the launch was refused, ending with the preview's address.
	Error string `json:"error"`
	// PreviewURL is the interface page that previews the inventory's hosts, empty when the run
	// named no stored inventory.
	PreviewURL string `json:"preview_url,omitempty"`
}

// inventoryPreviewPath is the interface page that opens the host preview of a stored inventory.
func inventoryPreviewPath(inventoryID string) string {
	return "/ui/inventories?preview=" + url.QueryEscape(inventoryID)
}

// respondNoHosts refuses a manual launch whose composed inventory resolved to no hosts, with the
// reason and a link to the inventory's host preview. A person launching can act on it at once, by
// widening the filter or the inputs, so the answer says where to look rather than only that nothing
// matched.
func respondNoHosts(w http.ResponseWriter, log *zap.Logger, inventoryID string, err error) {
	resp := noHostsResponse{Error: err.Error()}
	if inventoryID != "" {
		resp.PreviewURL = inventoryPreviewPath(inventoryID)
		resp.Error += ". See which hosts it matches now: " + resp.PreviewURL
	}
	respondJSON(w, log, http.StatusBadRequest, resp, false)
}

// hookSkipRecord is the canonical body a webhook skip's chain entry commits: which trigger fired,
// what it launched, and why it started nothing.
type hookSkipRecord struct {
	// TriggerID identifies the webhook trigger.
	TriggerID string `json:"trigger_id"`
	// TemplateID is the template the delivery launched.
	TemplateID string `json:"template_id,omitempty"`
	// InventoryID is the composed inventory that resolved to no hosts.
	InventoryID string `json:"inventory_id,omitempty"`
	// Reason is why the delivery started nothing.
	Reason string `json:"reason"`
}

// recordHookSkip appends the chain entry for a webhook delivery that was skipped because its
// template's inventory matched no hosts. Without a configured chain there is nothing to append to.
func recordHookSkip(ctx context.Context, audits audit.Store, tg *trigger.Trigger, t *template.Template) error {
	if audits == nil {
		return nil
	}
	body, err := json.Marshal(hookSkipRecord{
		TriggerID: tg.ID, TemplateID: t.ID, InventoryID: t.InventoryID, Reason: schedule.SkipNoHosts,
	})
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return err
	}
	return audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + tg.ID,
		Method: http.MethodPost, Path: "/hooks/" + tg.ID + "/skipped",
		ContentDigest: digest, Nonce: nonce,
	})
}

// hookSkipAnswer records a webhook delivery that was skipped and returns what its sender is told.
// The answer is 200 with the reason, never an error, because nothing went wrong: a sender that saw a
// failure would deliver again, and the same inventory would match nothing again. A skip the chain
// will not take is refused instead, the way the fire itself is, so a skip never exists without its
// evidence. It writes nothing to the request that delivered the event, since the launch it follows
// runs apart from that request.
func hookSkipAnswer(ctx context.Context, log *zap.Logger, audits audit.Store, tg *trigger.Trigger,
	t *template.Template) hookAnswer {
	if err := recordHookSkip(ctx, audits, tg, t); err != nil {
		log.Error("server: record webhook skip: " + err.Error())
		return hookAnswer{status: http.StatusServiceUnavailable,
			message: "refused: the webhook skip could not be recorded in the audit trail"}
	}
	log.Info("server: webhook fire skipped: "+schedule.SkipNoHosts,
		zap.String("trigger_id", tg.ID), zap.String("inventory_id", t.InventoryID))
	return hookAnswer{status: http.StatusOK,
		body: map[string]string{"trigger": tg.ID, "skipped": schedule.SkipNoHosts}}
}

// scheduleSkipFinding is the doctor's warning for a schedule whose recent fires were all skipped
// because its inventory matched no hosts. It names the inventory and links to its host preview,
// which is where the fix starts: a filter or an input that no longer reaches anything.
func scheduleSkipFinding(ctx context.Context, s *schedule.Schedule, templates template.Store,
	invs inventory.Store) doctorFinding {
	problem := "Matched no hosts for the last " + strconv.Itoa(s.SkippedFires) + " fires, so none " +
		"of them started a run."
	fix := "/ui/schedules"
	if inventoryID := scheduleInventory(ctx, s, templates); inventoryID != "" {
		name := inventoryID
		if invs != nil {
			if inv, err := invs.Get(ctx, inventoryID); err == nil && inv.Name != "" {
				name = inv.Name
			}
		}
		fix = inventoryPreviewPath(inventoryID)
		problem += " Its inventory " + strconv.Quote(name) + " resolves to no hosts. Preview it at " +
			fix + "."
	}
	return doctorFinding{
		Severity: "warning", ObjectType: "schedule", ObjectID: s.ID,
		ObjectName: namedOr(s.Name, s.ID), Problem: problem, FixPath: fix,
	}
}

// scheduleInventory returns the stored inventory a schedule's fires target, read from the template
// it fires, or the empty string when it fires none or the template cannot be read.
func scheduleInventory(ctx context.Context, s *schedule.Schedule, templates template.Store) string {
	if s.TemplateID == "" || templates == nil {
		return ""
	}
	t, err := templates.Get(ctx, s.TemplateID)
	if err != nil {
		return ""
	}
	return t.InventoryID
}
