package decision

import "strings"

// CorrectionPath is the chain path a correction is recorded at. It names the run first, so
// everything that collects a run's entries collects it, then the decision it corrects.
func CorrectionPath(runID, decisionID, correctionID string) string {
	return "/runs/" + runID + "/decisions/" + decisionID + "/corrections/" + correctionID
}

// RedactionPath is the chain path a reason's redaction is recorded at, naming the run, the decision
// it belongs to, the correction when it is a correction's text that was removed, and the category.
// The category is in the path so the reason a redaction gives is readable in every export without
// the text it removed.
func RedactionPath(runID, decisionID, correctionID, category string) string {
	path := "/runs/" + runID + "/decisions/" + decisionID
	if correctionID != "" {
		path += "/corrections/" + correctionID
	}
	return path + "/reason_redacted/" + category
}

// ReasonEntry is one entry of the REASON method read back from its path.
type ReasonEntry struct {
	// RunID is the run.
	RunID string
	// DecisionID is the decision the entry concerns.
	DecisionID string
	// CorrectionID is the correction concerned, empty when the entry concerns the decision itself.
	CorrectionID string
	// Redacted reports a redaction rather than a correction.
	Redacted bool
	// Category is the redaction's category, empty for a correction.
	Category string
}

// ParseReasonPath reads a path written by CorrectionPath or RedactionPath, reporting ok=false for
// anything else.
func ParseReasonPath(path string) (ReasonEntry, bool) {
	rest, found := strings.CutPrefix(path, "/runs/")
	if !found {
		return ReasonEntry{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] != "decisions" || parts[2] == "" {
		return ReasonEntry{}, false
	}
	e := ReasonEntry{RunID: parts[0], DecisionID: parts[2]}
	tail := parts[3:]
	if len(tail) >= 2 && tail[0] == "corrections" && tail[1] != "" {
		e.CorrectionID = tail[1]
		tail = tail[2:]
	}
	switch {
	case len(tail) == 0 && e.CorrectionID != "":
		return e, true
	case len(tail) == 2 && tail[0] == "reason_redacted" && ValidCategory(tail[1]):
		e.Redacted, e.Category = true, tail[1]
		return e, true
	}
	return ReasonEntry{}, false
}
