package notification

import "errors"

var (
	// ErrNotFound is returned when a notification target does not exist.
	ErrNotFound = errors.New("notification target not found")
	// ErrAttachmentNotFound is returned when an attachment does not exist.
	ErrAttachmentNotFound = errors.New("notification attachment not found")
	// ErrDuplicate is returned when an attachment already ties the same target to the same object
	// for the same event.
	ErrDuplicate = errors.New("notification target is already attached for that event")
	// ErrEvent is returned for an event name that is not one a target can be attached for.
	ErrEvent = errors.New("unknown notification event")
	// ErrObject is returned for an object kind a target cannot be attached to, or an attachment that
	// names no object.
	ErrObject = errors.New("notification targets attach to templates, workflows, schedules, " +
		"projects, and organizations")
	// ErrSealing is returned when a target carries a secret and no encryption key is configured to
	// seal it with. A secret is never stored in the clear instead.
	ErrSealing = errors.New("notification secrets need SWITCHTENDER_ENCRYPTION_KEY and " +
		"SWITCHTENDER_ENCRYPTION_SALT set on the server")
	// ErrNeedsSecret is returned when a target imported without its secret is asked for one to
	// deliver to.
	ErrNeedsSecret = errors.New("notification target needs its secret entered before it delivers")
	// ErrCleanupChanged is returned when the attachments a delete would remove are not the ones its
	// chain entry recorded, because one was made or removed after the entry was written. Nothing is
	// deleted.
	ErrCleanupChanged = errors.New("the notification attachments on this object changed while it " +
		"was being deleted, so nothing was deleted; try again")
	// ErrDeliveryLost is returned when a delivery's outcome is recorded by a worker whose claim on
	// it has lapsed or been taken over, so the outcome is not recorded.
	ErrDeliveryLost = errors.New("notification delivery is no longer claimed by this worker")
)
