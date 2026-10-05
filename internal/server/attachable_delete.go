package server

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/notification"
)

// attachableRoutes maps the collection a delete names to the notification object kind its objects
// are attached under. They are exactly the objects whose delete routes take one id.
var attachableRoutes = map[string]string{
	"templates": notification.KindTemplate,
	"schedules": notification.KindSchedule,
	"projects":  notification.KindProject,
	"orgs":      notification.KindOrg,
}

// attachableDelete returns the notification object kind and id a request deletes, and whether it
// deletes one: a DELETE of exactly /v1/{collection}/{id} on a collection whose objects targets
// attach to. A sub-resource, such as a template's callback key, is not the object itself.
func attachableDelete(r *http.Request) (kind, id string, ok bool) {
	if r.Method != http.MethodDelete {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[2] == "" {
		return "", "", false
	}
	kind, ok = attachableRoutes[parts[1]]
	return kind, parts[2], ok
}

// attachmentCleanups returns what the gate asks, before it records a delete, about the
// notification attachments the delete takes with it, so the delete's own chain entry can state the
// cleanup rather than it being recorded as events of its own. It reports nil for a request that
// deletes no attachable object, and for an install with no notification targets.
func attachmentCleanups(store notification.Store) func(context.Context,
	*http.Request) (*notification.Cleanup, error) {
	return func(ctx context.Context, r *http.Request) (*notification.Cleanup, error) {
		kind, id, ok := attachableDelete(r)
		if !ok || store == nil {
			return nil, nil
		}
		attached, err := store.AttachedTo(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		targets := make([]string, 0, len(attached))
		for _, a := range attached {
			targets = append(targets, a.NotificationID)
		}
		c := notification.Summarize(targets)
		return &c, nil
	}
}

// cleanupHolder carries the attachment cleanup the gate recorded for a request from the gate to
// the handler that performs the delete.
type cleanupHolder struct {
	// mu guards cleanup and set.
	mu sync.Mutex
	// cleanup is what the request's chain entry states the delete removes.
	cleanup notification.Cleanup
	// set reports that the gate recorded one.
	set bool
}

// cleanupHolderKey is the context key the holder rides under.
type cleanupHolderKey struct{}

// withCleanupHolder returns ctx carrying an empty holder for the gate to fill.
func withCleanupHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, cleanupHolderKey{}, &cleanupHolder{})
}

// noteRecordedCleanup stores the cleanup the gate recorded on the request's holder, when there is
// one.
func noteRecordedCleanup(ctx context.Context, c notification.Cleanup) {
	h, ok := ctx.Value(cleanupHolderKey{}).(*cleanupHolder)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanup, h.set = c, true
}

// recordedCleanupFrom returns the cleanup the gate recorded for this request, and whether it
// recorded one.
func recordedCleanupFrom(ctx context.Context) (notification.Cleanup, bool) {
	h, ok := ctx.Value(cleanupHolderKey{}).(*cleanupHolder)
	if !ok {
		return notification.Cleanup{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleanup, h.set
}

// objectDeleter is the delete every store of an attachable object has.
type objectDeleter interface {
	// Delete removes the object with the given id.
	Delete(ctx context.Context, id string) error
}

// deleteAttachableObject deletes an object a notification target can be attached to. When the
// request's chain entry recorded the attachments the delete removes, a store that can be held to
// that record is, so the delete happens exactly as recorded or not at all. Every database store
// removes an object's attachments in the object's own delete either way.
func deleteAttachableObject(ctx context.Context, store objectDeleter, id string) error {
	if recorded, ok := recordedCleanupFrom(ctx); ok {
		if rd, ok := store.(notification.RecordedDeleter); ok {
			return rd.DeleteRecorded(ctx, id, recorded)
		}
	}
	return store.Delete(ctx, id)
}

// respondCleanupChanged writes the refusal for a delete whose attachments changed after its chain
// entry was written, reporting whether err was that.
func respondCleanupChanged(w http.ResponseWriter, log *zap.Logger, err error) bool {
	if !errors.Is(err, notification.ErrCleanupChanged) {
		return false
	}
	respondError(w, log, http.StatusConflict, err.Error())
	return true
}
