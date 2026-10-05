package importer

import "github.com/kordloom/switchtender/internal/schedule"

// awxSpringForward returns the spring-forward setting an AWX schedule carries. A rule kept as a
// rule takes a rule's default, which is AWX's own reading of a time the clocks skip: after the
// clock change, the length of the jump later.
// A rule carried as a cron expression would take the jump a cron expression defaults to, and fire
// half an hour or an hour earlier than AWX did on the one night a year it matters, so it names
// AWX's reading explicitly.
func awxSpringForward(cron string) string {
	if cron == "" {
		return ""
	}
	return schedule.SpringForwardLater
}
