package notification

// hears reports whether an attachment is told about an event. An attachment hears the event it was
// made for. A skipped schedule fire is also heard by an attachment on the schedule for failure,
// because an attachment imported from AWX never names skipped, and whoever watches a schedule's
// failures is who needs to know it stopped reaching hosts. Started, success, and approval
// attachments do not hear a skip, and a failure attachment on anything but the schedule does not
// either.
func hears(a *Attachment, event string) bool {
	if a.Event == event {
		return true
	}
	return event == EventSkipped && a.ObjectKind == KindSchedule && a.Event == EventFailure
}
