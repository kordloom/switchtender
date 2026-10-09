package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// healthDeliveries bounds how many of a target's recent deliveries its status is read from.
const healthDeliveries = 200

// notificationView is a target as the API answers it: the stored target, never its secrets, with
// its delivery status beside it.
type notificationView struct {
	*notification.Notification
	// Delivery is the target's delivery status: needs_secret, configured, healthy, retrying, or
	// failing, what is missing when it waits for its secret, and what its recent deliveries came
	// to.
	Delivery notification.Health `json:"delivery"`
}

// viewNotification reads a target's recent deliveries and returns it with its delivery status. A
// status that cannot be read leaves the target answered with what the target itself says, since
// the target is still there and its record is what the caller asked for.
func viewNotification(ctx context.Context, store notification.Store, n *notification.Notification,
	log *zap.Logger) notificationView {
	since := time.Now().Add(-notification.HealthWindow)
	recent, err := store.Deliveries(ctx, notification.DeliveryFilter{NotificationID: n.ID,
		Limit: healthDeliveries})
	if err != nil {
		log.Error("server: read notification deliveries: "+err.Error(),
			zap.String("notification_id", n.ID))
		recent = nil
	}
	return notificationView{Notification: n, Delivery: notification.HealthOf(n, recent, since)}
}

// deliveriesResponse wraps a delivery list.
type deliveriesResponse struct {
	// Deliveries are the deliveries, newest first.
	Deliveries []*notification.Delivery `json:"deliveries"`
	// Count is the number returned.
	Count int `json:"count"`
}

// targetDeliveriesHandler returns a target's recent deliveries, newest first: each event of each
// run it was told about, and whether it arrived.
func targetDeliveriesHandler(store notification.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		n, ok := loadNotification(w, r, store, log)
		if !ok {
			return
		}
		if denyOnAuthzError(w, log, authz.authorize(r.Context(), n.ID, grant.AccessRead)) {
			return
		}
		list, err := store.Deliveries(r.Context(), notification.DeliveryFilter{NotificationID: n.ID,
			Status: r.URL.Query().Get("status"), Limit: notification.DefaultDeliveryLimit})
		if err != nil {
			log.Error("server: list notification deliveries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list deliveries")
			return
		}
		respondJSON(w, log, http.StatusOK, deliveriesResponse{Deliveries: list, Count: len(list)},
			wantsPretty(r))
	}
}

// runNotificationsHandler returns what the named notification targets were told about a run, in
// the run's event order: every event, every target, and whether each delivery arrived, is still
// on its way, failed for good, or was skipped, with why.
func runNotificationsHandler(runs run.Store, store notification.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rn, err := runs.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: read run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		list := []*notification.Delivery{}
		if store != nil {
			list, err = store.Deliveries(r.Context(), notification.DeliveryFilter{RunID: rn.ID,
				Limit: notification.DefaultDeliveryLimit})
			if err != nil {
				log.Error("server: list run notifications: " + err.Error())
				respondError(w, log, http.StatusInternalServerError,
					"could not list the run's notifications")
				return
			}
		}
		// The store lists newest first; a run's page reads its events in the order they happened.
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
		respondJSON(w, log, http.StatusOK, deliveriesResponse{Deliveries: list, Count: len(list)},
			wantsPretty(r))
	}
}

// notificationDoctor returns the doctor's check of the named notification targets: a target waiting
// for its secret, a target that failed to deliver within the window, and an attachment whose
// target or object is gone, from whatever path it was left behind.
func notificationDoctor(store notification.Store, objects notificationObjects,
	log *zap.Logger) doctorCheck {
	return func(ctx context.Context) (doctorCheckResult, error) {
		var res doctorCheckResult
		if store == nil {
			return res, nil
		}
		targets, err := store.List(ctx)
		if err != nil {
			return res, fmt.Errorf("list notification targets: %w", err)
		}
		names := make(map[string]string, len(targets))
		for _, n := range targets {
			names[n.ID] = namedOr(n.Name, n.ID)
			res.Findings = append(res.Findings, targetFindings(ctx, store, n, log)...)
		}
		attachments, err := store.ListAttachments(ctx)
		if err != nil {
			return res, fmt.Errorf("list notification attachments: %w", err)
		}
		for _, a := range attachments {
			found, err := orphanFinding(ctx, objects, names, a)
			if err != nil {
				return res, err
			}
			if found != nil {
				res.Findings = append(res.Findings, *found)
			}
		}
		res.Checked = map[string]int{"notification_targets": len(targets),
			"notification_attachments": len(attachments)}
		return res, nil
	}
}

// targetFindings reports what is wrong with one target: still waiting for its secret, or failing
// to deliver.
func targetFindings(ctx context.Context, store notification.Store, n *notification.Notification,
	log *zap.Logger) []doctorFinding {
	add := func(problem string) doctorFinding {
		return doctorFinding{Severity: "warning", ObjectType: "notification", ObjectID: n.ID,
			ObjectName: namedOr(n.Name, n.ID), Problem: problem, FixPath: "/ui/notifications"}
	}
	view := viewNotification(ctx, store, n, log)
	var out []doctorFinding
	if n.NeedsSecret {
		out = append(out, add("Is waiting for its secret, "+missingWords(view.Delivery.Missing)+
			", so it delivers nothing. It came from an import that could not carry the secret."))
	}
	if h := view.Delivery; h.Failed > 0 && h.LastFailedAt != nil {
		problem := fmt.Sprintf("Failed to deliver %d %s in the last seven days. The latest failed "+
			"at %s: %s.", h.Failed, util.Plural(h.Failed, "notification", "notifications"),
			h.LastFailedAt.UTC().Format(time.RFC3339), h.LastError)
		if h.State == notification.StateHealthy || h.State == notification.StateRetrying {
			problem += " It has delivered since."
		}
		out = append(out, add(problem))
	}
	return out
}

// missingWords names the secret parts a target lacks, for a sentence.
func missingWords(missing []string) string {
	switch {
	case len(missing) == 2:
		return "its address and its key"
	case len(missing) == 1 && missing[0] == notification.PartURL:
		return "its address"
	case len(missing) == 1:
		return "its key"
	}
	return "its secret"
}

// orphanFinding reports an attachment whose target or object no longer exists, or nil when both
// do. An object kind whose store is not configured cannot be checked, and is not reported.
func orphanFinding(ctx context.Context, objects notificationObjects, names map[string]string,
	a *notification.Attachment) (*doctorFinding, error) {
	finding := func(problem string) *doctorFinding {
		name, ok := names[a.NotificationID]
		if !ok {
			name = a.NotificationID
		}
		return &doctorFinding{Severity: "warning", ObjectType: "notification attachment",
			ObjectID: a.ID, ObjectName: name, Problem: problem, FixPath: "/ui/notifications"}
	}
	if _, ok := names[a.NotificationID]; !ok {
		return finding("Attaches target " + a.NotificationID + ", which does not exist, to " +
			a.ObjectKind + " " + a.ObjectID + "."), nil
	}
	if !objects.checks(a.ObjectKind) {
		return nil, nil
	}
	found, err := objects.exists(ctx, a.ObjectKind, a.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("read the object an attachment names: %w", err)
	}
	if found {
		return nil, nil
	}
	return finding("Is attached to " + a.ObjectKind + " " + a.ObjectID + ", which does not " +
		"exist, so it tells nobody anything. Detach it."), nil
}

// checks reports whether the store an object kind is read from is configured, so its attachments
// can be checked.
func (o notificationObjects) checks(kind string) bool {
	switch kind {
	case notification.KindTemplate:
		return o.templates != nil
	case notification.KindSchedule:
		return o.schedules != nil
	case notification.KindProject:
		return o.projects != nil
	case notification.KindOrg:
		return o.orgs != nil
	}
	return false
}

// notificationObjects returns the stores the objects a notification target attaches to are read
// from.
func (s *Server) notificationObjects() notificationObjects {
	return notificationObjects{templates: s.templates, schedules: s.schedules,
		projects: s.projects, orgs: s.orgs}
}
