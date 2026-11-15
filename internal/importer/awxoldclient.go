package importer

import (
	"bytes"
	"encoding/json"
)

// awxClientProbe is the part of an AWX export the old-client check reads as the file holds it,
// before any of it is mapped.
type awxClientProbe struct {
	// Schedules is the top-level schedules value, kept raw so a list can be told apart from a null
	// and from no key at all.
	Schedules json.RawMessage `json:"schedules"`
	// Workflows are the workflow job templates, read only for the key awxkit writes on each.
	Workflows []awxProbeWorkflow `json:"workflow_job_templates"`
}

// awxProbeWorkflow is one workflow job template as the old-client check reads it.
type awxProbeWorkflow struct {
	// NaturalKey is the key awxkit identifies every object it exports by. A hand-written file and a
	// REST API dump carry none.
	NaturalKey json.RawMessage `json:"natural_key"`
}

// warnOldAWXClient says so in the preview when an export looks like one written by an awx client
// older than 21.4.0, the client of the Tower 3.8 era.
//
// That client cannot write an approval node. awxkit before 21.4.0 has no page for a workflow
// approval template, so the node's template has no natural key and the export drops the node whole.
// An approval in the middle of a graph leaves a trace, since the node before it still points at it
// and the workflow is refused for an edge to a missing node. An approval at the start leaves none:
// the work it guarded and its deny path both arrive as steps with no dependency, and they start
// together on the first run. Nothing in the file says the gate was there, so this check is the only
// notice the operator gets.
//
// The file is judged by three things awxkit writes. It holds workflows carrying awxkit's natural
// key. None of them holds an approval node, which a client of 21.4.0 or later would have written.
// And it has no top-level schedules list, which awxkit has written on a full export since 21.3.0. A
// partial export from a newer client can match all three, which is why the warning is worded as a
// check. An export from awxkit 21.3.0 itself cannot be told apart from a later one, since that one
// release writes the schedules list and still drops approvals, so this check does not catch it.
func (p *Plan) warnOldAWXClient(data []byte, export awxExport) {
	if !looksLikeOldAWXClient(data, export) {
		return
	}
	p.warn("this export looks like one written by the awx client of the Tower 3.8 era, upstream " +
		"awxkit before 21.4.0: its workflows hold no approval node and it has no top-level " +
		"schedules list. That client leaves approval nodes out of its export, so a workflow that " +
		"waited for an approval can arrive with no gate. Check each workflow against the source " +
		"and add any missing approval back as an approval step.")
}

// looksLikeOldAWXClient reports whether an export matches the three signs warnOldAWXClient judges
// it by.
func looksLikeOldAWXClient(data []byte, export awxExport) bool {
	var probe awxClientProbe
	// The document already decoded as a whole before this runs. A type error here leaves every field
	// that did decode in place and marks only a value no awxkit writes, so it changes nothing this
	// check reads and is not a reason to skip it.
	_ = json.Unmarshal(data, &probe)
	if bytes.HasPrefix(bytes.TrimSpace(probe.Schedules), []byte("[")) {
		return false
	}
	fromAwxkit := false
	for _, wf := range probe.Workflows {
		if key := bytes.TrimSpace(wf.NaturalKey); len(key) > 0 && !bytes.Equal(key, []byte("null")) {
			fromAwxkit = true
			break
		}
	}
	if !fromAwxkit {
		return false
	}
	for _, wf := range export.Workflows {
		for _, n := range wf.nodes() {
			if n.approvalGate() != nil {
				return false
			}
		}
	}
	return true
}
