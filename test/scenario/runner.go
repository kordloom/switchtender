package scenario

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// RunCase drives one case through a built install and returns every expectation it failed.
//
// It collects failures rather than stopping at the first, because a case is a sequence and the
// later steps say whether the install ended where the case claimed. Stopping early turns one
// wrong status into no information about anything after it.
func RunCase(in *Install, c Case) []string {
	var out []string
	for i, step := range c.Steps {
		method, path, err := step.Verb()
		if err != nil {
			out = append(out, fmt.Sprintf("step %d: %v", i, err))
			continue
		}
		before, beforeErr := auditLength(in, method)
		res := issue(in, method, path, step)
		failures := checkExpect(in, step, res)
		failures = append(failures, checkWriteRecorded(in, method, before, beforeErr)...)
		for _, failure := range failures {
			out = append(out, fmt.Sprintf("step %d (%s %s as %s%s): %s",
				i, method, path, actorLabel(step.As), noteSuffix(step.Note), failure))
		}
	}
	return out
}

// issue sends one step's request, with whichever body form it declared.
func issue(in *Install, method, path string, step Step) Response {
	switch {
	case step.RawBody != "":
		return in.Post(path, step.As, []byte(step.RawBody))
	case step.Body != nil:
		body, err := json.Marshal(step.Body)
		if err != nil {
			return Response{Status: 0, Body: fmt.Sprintf("could not encode step body: %v", err)}
		}
		return in.Post(path, step.As, body)
	default:
		return in.Do(method, path, step.As, nil)
	}
}

// auditLength reads the chain before a write, so the entry that write should append can be looked
// for afterward. Reads are not measured, since they are not what the chain records.
func auditLength(in *Install, method string) (int, error) {
	if method != http.MethodPost || in.auditCount == nil {
		return 0, nil
	}
	return in.auditCount()
}

// checkWriteRecorded requires every write to leave an entry in the audit chain.
//
// Both answers count. A refused write is recorded too, and that is deliberate: a trail that holds
// only what succeeded cannot answer what was attempted, which is most of what an investigation
// asks. So this does not look at the status, only at whether the chain grew.
func checkWriteRecorded(in *Install, method string, before int, beforeErr error) []string {
	if method != http.MethodPost || in.auditCount == nil {
		return nil
	}
	if _, skipped := in.Scenario.SkipInvariants["writes_are_recorded"]; skipped {
		return nil
	}
	if beforeErr != nil {
		return []string{fmt.Sprintf("could not read the audit chain before the write: %v", beforeErr)}
	}
	after, err := in.auditCount()
	if err != nil {
		return []string{fmt.Sprintf("could not read the audit chain after the write: %v", err)}
	}
	if after > before {
		return nil
	}
	return []string{fmt.Sprintf(
		"the audit chain still holds %d entries, so this write left no record. A chain with an "+
			"entry missing from the start verifies perfectly, so nothing downstream can detect it.",
		after)}
}

// checkExpect holds one response, and the state behind it, against what the step declared.
func checkExpect(in *Install, step Step, res Response) []string {
	var out []string
	e := step.Expect
	if e.Status != 0 && res.Status != e.Status {
		out = append(out, fmt.Sprintf("status %d, want %d (%s)",
			res.Status, e.Status, firstLine(res.Body)))
	}
	for _, want := range e.Includes {
		if !strings.Contains(res.Body, want) {
			out = append(out, fmt.Sprintf("body does not contain %q (%s)", want, firstLine(res.Body)))
		}
	}
	for _, unwanted := range e.Excludes {
		if strings.Contains(res.Body, unwanted) {
			out = append(out, fmt.Sprintf("body contains %q, which it must not (%s)",
				unwanted, firstLine(res.Body)))
		}
	}
	for _, check := range e.State {
		if failure := checkState(in, check); failure != "" {
			out = append(out, failure)
		}
	}
	return out
}

// checkState holds the stored state against what the step declared, which is how a write that
// answers 200 and persists nothing is caught. A response is what the server said; this is what it
// did.
func checkState(in *Install, check StateCheck) string {
	if check.Runs == nil {
		return ""
	}
	count, err := in.runCount(check.Runs.Project)
	if err != nil {
		return fmt.Sprintf("could not read the runs the install holds: %v", err)
	}
	if count != check.Runs.Count {
		where := "the store"
		if check.Runs.Project != "" {
			where = "project " + check.Runs.Project
		}
		return fmt.Sprintf("%s holds %d run(s), want %d", where, count, check.Runs.Count)
	}
	return ""
}

// actorLabel names who a step acted as, for a failure message.
func actorLabel(as string) string {
	if as == "" {
		return "nobody"
	}
	return as
}

// noteSuffix renders a step's note when it has one.
func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}
