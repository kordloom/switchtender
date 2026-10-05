package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/util"
)

// notificationRequest is the JSON body accepted by POST /notifications and PUT /notifications/{id}.
type notificationRequest struct {
	// Name labels the target. Required.
	Name string `json:"name"`
	// Description says what the target is for. A pointer so an edit that omits it keeps it.
	Description *string `json:"description,omitempty"`
	// Kind is the channel. Required on create; blank on an edit keeps the stored kind.
	Kind string `json:"kind,omitempty"`
	// URL is the address a URL-configured channel posts to, or Grafana's base URL. On an edit, blank
	// or the masked hint a read returned keeps the stored address. Never echoed back.
	URL string `json:"url,omitempty"`
	// Key is a PagerDuty routing key or a Grafana token. On an edit, blank or the mask keeps the
	// stored key. Never echoed back.
	Key string `json:"key,omitempty"`
	// To is the recipient a twilio or email target names. A pointer so an edit that omits it keeps
	// it.
	To *string `json:"to,omitempty"`
	// OrgID names the owning organization. A pointer so an edit that omits it keeps the owner.
	OrgID *string `json:"org_id,omitempty"`
}

// notificationsResponse wraps a target list.
type notificationsResponse struct {
	// Notifications is the list of targets, secrets excluded, each with its delivery status.
	Notifications []notificationView `json:"notifications"`
	// Count is the number returned.
	Count int `json:"count"`
	// Total is how many exist before the response cap.
	Total int `json:"total"`
	// Sealing reports whether this install can store a target's secret, which needs the encryption
	// key and salt, so a form can say so before anybody pastes a webhook address into it.
	Sealing bool `json:"sealing"`
}

// attachRequest is the JSON body accepted by POST /notifications/{id}/attachments.
type attachRequest struct {
	// ObjectKind is what the target is attached to: template, workflow, schedule, project, or org.
	ObjectKind string `json:"object_kind"`
	// ObjectID is the object's id.
	ObjectID string `json:"object_id"`
	// Event is when the target is told: started, success, failure, approval, or skipped.
	Event string `json:"event"`
}

// attachmentsResponse wraps an attachment list.
type attachmentsResponse struct {
	// Attachments is the list of attachments.
	Attachments []*notification.Attachment `json:"attachments"`
	// Count is the number returned.
	Count int `json:"count"`
}

// notificationObjects reads the objects a target can be attached to, so an attachment naming one
// that does not exist is refused rather than stored to wait for an id that will never come.
type notificationObjects struct {
	// templates reads templates and workflows.
	templates template.Store
	// schedules reads schedules.
	schedules schedule.Store
	// projects reads projects.
	projects project.Store
	// orgs reads organizations.
	orgs org.Store
}

// exists reports whether the object an attachment names exists. A kind whose store is not
// configured cannot be checked, and is refused rather than accepted on faith.
func (o notificationObjects) exists(ctx context.Context, kind, id string) (bool, error) {
	var err error
	switch {
	case kind == notification.KindTemplate && o.templates != nil:
		_, err = o.templates.Get(ctx, id)
	case kind == notification.KindSchedule && o.schedules != nil:
		_, err = o.schedules.Get(ctx, id)
	case kind == notification.KindProject && o.projects != nil:
		_, err = o.projects.Get(ctx, id)
	case kind == notification.KindOrg && o.orgs != nil:
		_, err = o.orgs.Get(ctx, id)
	default:
		return false, nil
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, template.ErrNotFound), errors.Is(err, schedule.ErrNotFound),
		errors.Is(err, project.ErrNotFound), errors.Is(err, org.ErrNotFound):
		return false, nil
	}
	return false, err
}

// notificationSealer adapts the credential sealer for the notification package, keeping a nil
// sealer nil rather than a typed nil the package would call through.
func notificationSealer(s *credential.Sealer) notification.Sealer {
	if s == nil {
		return nil
	}
	return s
}

// masked reports whether a submitted secret is blank or is the mask a read handed out, either of
// which means keep the stored value. Accepting the mask as a value would point the channel at the
// redaction itself.
func masked(v string) bool {
	return strings.TrimSpace(v) == "" || strings.Contains(v, util.MaskMarker)
}

// respondNotificationError writes the response for a refused target configuration.
func respondNotificationError(w http.ResponseWriter, log *zap.Logger, err error) {
	if errors.Is(err, notification.ErrSealing) {
		respondError(w, log, http.StatusConflict, err.Error())
		return
	}
	respondError(w, log, http.StatusBadRequest, err.Error())
}

// createNotificationHandler seals and stores a new named notification target.
func createNotificationHandler(store notification.Store, sealer *credential.Sealer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		var req notificationRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		// Putting a target in an organization lets every member of it attach the target, so entering
		// one is checked by membership, the same as a credential.
		if authz.denyForeignOrg(w, r, log, orgForCreate(req.OrgID)) {
			return
		}
		n := &notification.Notification{
			ID: notification.NewID(), Name: strings.TrimSpace(req.Name),
			OrgID: orgForCreate(req.OrgID), CreatedAt: time.Now(), CreatedBy: actorName(r),
		}
		if req.Description != nil {
			n.Description = *req.Description
		}
		to := ""
		if req.To != nil {
			to = *req.To
		}
		err := n.SetTarget(run.NotifyTarget{Kind: req.Kind, URL: req.URL, Key: req.Key, To: to},
			notificationSealer(sealer))
		req.URL, req.Key = "", ""
		if err != nil {
			respondNotificationError(w, log, err)
			return
		}
		if err := store.Save(r.Context(), n); err != nil {
			log.Error("server: save notification target: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save notification target")
			return
		}
		w.Header().Set("Location", "/v1/notifications/"+n.ID)
		respondJSON(w, log, http.StatusCreated, viewNotification(r.Context(), store, n, log),
			wantsPretty(r))
	}
}

// updateNotificationHandler edits a target. A blank or masked address or key keeps the stored one,
// so a target can be renamed or retargeted at a new recipient without re-entering its secret, and
// a stored secret never travels back through a form to be kept. An imported target waiting for its
// secret keeps every part it has, so it is completed by sending only what is missing; an edit that
// still leaves something missing is saved and the target stays waiting.
func updateNotificationHandler(store notification.Store, sealer *credential.Sealer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		var req notificationRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		existing, ok := loadNotification(w, r, store, log)
		if !ok {
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		owner := orgForUpdate(req.OrgID, existing.OrgID)
		if owner != existing.OrgID && authz.denyForeignOrg(w, r, log, owner) {
			return
		}
		seal := notificationSealer(sealer)
		// The stored secrets are opened only to be sealed again beside whatever changed, so one
		// path validates every edit the way a create is validated. A target waiting for its secret
		// opens the parts it does have, which an import kept so that finishing it asks only for
		// the one that is missing.
		current, err := existing.Known(seal)
		if err != nil {
			respondNotificationError(w, log, err)
			return
		}
		next := current
		if req.Kind != "" && req.Kind != current.Kind {
			// A different kind reads its address and key differently, so nothing carries over.
			next = run.NotifyTarget{Kind: req.Kind}
		}
		if !masked(req.URL) {
			next.URL = req.URL
		}
		if !masked(req.Key) {
			next.Key = req.Key
		}
		if req.To != nil {
			next.To = *req.To
		}
		updated := existing.Clone()
		updated.Name = strings.TrimSpace(req.Name)
		updated.OrgID = owner
		if req.Description != nil {
			updated.Description = *req.Description
		}
		if existing.NeedsSecret && len(notification.MissingParts(next)) > 0 {
			err = updated.SetKnown(next, seal)
		} else {
			err = updated.SetTarget(next, seal)
		}
		req.URL, req.Key, current.URL, current.Key, next.URL, next.Key = "", "", "", "", "", ""
		if err != nil {
			respondNotificationError(w, log, err)
			return
		}
		if err := store.Update(r.Context(), updated); errors.Is(err, notification.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "notification target not found")
			return
		} else if err != nil {
			log.Error("server: update notification target: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save notification target")
			return
		}
		respondJSON(w, log, http.StatusOK, viewNotification(r.Context(), store, updated, log),
			wantsPretty(r))
	}
}

// loadNotification reads the target the path names, writing the response and reporting false when
// it cannot.
func loadNotification(w http.ResponseWriter, r *http.Request, store notification.Store,
	log *zap.Logger) (*notification.Notification, bool) {
	n, err := store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, notification.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "notification target not found")
		return nil, false
	}
	if err != nil {
		log.Error("server: read notification target: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read notification target")
		return nil, false
	}
	return n, true
}

// listNotificationsHandler returns the targets the caller may read.
func listNotificationsHandler(store notification.Store, sealer *credential.Sealer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list notification targets: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list notification targets")
			return
		}
		visible, err := filterReadable(r.Context(), authz, list,
			func(n *notification.Notification) string { return n.ID },
			func(n *notification.Notification) string { return n.OrgID })
		if err != nil {
			log.Error("server: list notification targets: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list notification targets")
			return
		}
		capped, total := cappedList(visible)
		views := make([]notificationView, 0, len(capped))
		for _, n := range capped {
			views = append(views, viewNotification(r.Context(), store, n, log))
		}
		respondJSON(w, log, http.StatusOK, notificationsResponse{
			Notifications: views, Count: len(views), Total: total,
			Sealing: sealer != nil && sealer.Enabled(),
		}, wantsPretty(r))
	}
}

// getNotificationHandler returns one target the caller may read.
func getNotificationHandler(store notification.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		respondJSON(w, log, http.StatusOK, viewNotification(r.Context(), store, n, log),
			wantsPretty(r))
	}
}

// deleteNotificationHandler removes a target and every attachment it has, so no object keeps
// pointing at a channel that is gone.
func deleteNotificationHandler(store notification.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		id := r.PathValue("id")
		if err := store.Delete(r.Context(), id); errors.Is(err, notification.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "notification target not found")
			return
		} else if err != nil {
			log.Error("server: delete notification target: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete notification target")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": id}, wantsPretty(r))
	}
}

// listAttachmentsHandler returns the objects a target is attached to.
func listAttachmentsHandler(store notification.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		list, err := store.Attachments(r.Context(), n.ID)
		if err != nil {
			log.Error("server: list notification attachments: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list attachments")
			return
		}
		respondJSON(w, log, http.StatusOK, attachmentsResponse{Attachments: list, Count: len(list)},
			wantsPretty(r))
	}
}

// attachNotificationHandler attaches a target to an object for one event.
//
// Attaching a target decides where an object's runs are reported, which is part of managing that
// object, so it asks what editing the object asks and what using the target asks. The target needs
// use, the same as a template needs use to be launched. The object needs management: a manage grant
// on a template or project, or an admin of the organization that owns it. A schedule and an
// organization have no manage grant to hold, and editing either is admin work, so attaching to one
// is too.
func attachNotificationHandler(store notification.Store, objects notificationObjects, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		var req attachRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		n, ok := loadNotification(w, r, store, log)
		if !ok {
			return
		}
		kind, err := notification.NormalizeObjectKind(req.ObjectKind)
		if err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		event, err := notification.NormalizeEvent(req.Event)
		if err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		objectID := strings.TrimSpace(req.ObjectID)
		if objectID == "" {
			respondError(w, log, http.StatusBadRequest, "object_id is required")
			return
		}
		if denyOnAuthzError(w, log, authz.authorize(r.Context(), n.ID, grant.AccessUse)) {
			return
		}
		if !mayManageAttachmentObject(w, r, authz, log, kind, objectID) {
			return
		}
		found, err := objects.exists(r.Context(), kind, objectID)
		if err != nil {
			log.Error("server: read attachment object: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read "+kind)
			return
		}
		if !found {
			respondError(w, log, http.StatusNotFound, "no "+kind+" "+objectID+" exists to attach to")
			return
		}
		a := &notification.Attachment{
			ID: notification.NewAttachmentID(), NotificationID: n.ID, ObjectKind: kind,
			ObjectID: objectID, Event: event, CreatedAt: time.Now(), CreatedBy: actorName(r),
		}
		if err := store.Attach(r.Context(), a); errors.Is(err, notification.ErrDuplicate) {
			respondError(w, log, http.StatusConflict, err.Error())
			return
		} else if err != nil {
			log.Error("server: attach notification target: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not attach notification target")
			return
		}
		respondJSON(w, log, http.StatusCreated, a, wantsPretty(r))
	}
}

// detachNotificationHandler removes one attachment, asking what attaching it asked.
func detachNotificationHandler(store notification.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "notification targets not enabled")
			return
		}
		n, ok := loadNotification(w, r, store, log)
		if !ok {
			return
		}
		list, err := store.Attachments(r.Context(), n.ID)
		if err != nil {
			log.Error("server: list notification attachments: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read attachment")
			return
		}
		var found *notification.Attachment
		for _, a := range list {
			if a.ID == r.PathValue("attachment") {
				found = a
				break
			}
		}
		if found == nil {
			respondError(w, log, http.StatusNotFound, "attachment not found on this target")
			return
		}
		if denyOnAuthzError(w, log, authz.authorize(r.Context(), n.ID, grant.AccessUse)) {
			return
		}
		if !mayManageAttachmentObject(w, r, authz, log, found.ObjectKind, found.ObjectID) {
			return
		}
		err = store.Detach(r.Context(), found.ID)
		if errors.Is(err, notification.ErrAttachmentNotFound) {
			respondError(w, log, http.StatusNotFound, "attachment not found on this target")
			return
		}
		if err != nil {
			log.Error("server: detach notification target: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not detach notification target")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"detached": found.ID}, wantsPretty(r))
	}
}

// mayManageAttachmentObject reports whether the request actor may change where an object's runs are
// reported, writing the denial when not. An admin may. Below admin, a template or project needs an
// explicit manage grant or admin of its owning organization, the delegation that lets the same
// actor edit it, and a schedule or an organization is refused.
func mayManageAttachmentObject(w http.ResponseWriter, r *http.Request, authz *authorizer,
	log *zap.Logger, kind, objectID string) bool {
	actor, ok := actorFrom(r.Context())
	if !ok || actor.Role == user.RoleAdmin {
		return true
	}
	if actor.Agent {
		respondError(w, log, http.StatusForbidden,
			"an agent token cannot change where runs are reported")
		return false
	}
	if kind == notification.KindTemplate || kind == notification.KindProject {
		manages, err := authz.manages(r.Context(), actor, objectID)
		if err != nil {
			log.Error("server: check object management: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not check access")
			return false
		}
		if manages {
			return true
		}
	}
	respondError(w, log, http.StatusForbidden, "attaching a notification target to a "+kind+
		" needs management of that "+kind+": an admin, or a manage grant on it")
	return false
}
