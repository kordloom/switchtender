// Package plantest builds what a Terraform or OpenTofu plan returns to the executor, its saved plan
// file and the file's JSON rendering, for tests whose runner stands in for the tool.
package plantest

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/roundhouse"
)

// File is the stand-in plan file every Result carries. The executor seals it, binds its digest into
// the apply's approval, and hands it back to the tool, so its content only has to be the same bytes
// from one end to the other.
var File = []byte("switchtender test plan file")

// destroyCount reads the destroy count from a plan's change summary.
var destroyCount = regexp.MustCompile(`(?m)^Plan: .*?(\d+) to destroy`)

// noChanges reads the verdict a plan with nothing to do prints.
var noChanges = regexp.MustCompile(`(?m)^No changes\.`)

// JSON returns the JSON rendering of a plan that destroys the given number of resources, in the shape
// show -json prints: one resource change with a delete action for each.
func JSON(destroys int) []byte {
	type change struct {
		// Actions are the planned actions.
		Actions []string `json:"actions"`
	}
	type resourceChange struct {
		// Address names the resource.
		Address string `json:"address"`
		// Change is the planned change.
		Change change `json:"change"`
	}
	plan := struct {
		// FormatVersion is the rendering's format version.
		FormatVersion string `json:"format_version"`
		// ResourceChanges are the planned resource changes.
		ResourceChanges []resourceChange `json:"resource_changes"`
	}{FormatVersion: "1.2", ResourceChanges: []resourceChange{}}
	for i := range destroys {
		plan.ResourceChanges = append(plan.ResourceChanges, resourceChange{
			Address: "terraform_data.r" + strconv.Itoa(i), Change: change{Actions: []string{"delete"}},
		})
	}
	b, err := json.Marshal(plan)
	if err != nil {
		panic("plantest: marshal plan: " + err.Error())
	}
	return b
}

// Result returns the runner result of a plan that printed summary and succeeded: drift reported, the
// plan file saved, and its rendering destroying as many resources as the summary says. A summary that
// says nothing changes renders a plan that destroys nothing, and one that states no count at all
// renders nothing, which the executor treats as a plan nobody could measure.
func Result(summary string) roundhouse.Result {
	res := roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: append([]byte(nil), File...)}
	switch m := destroyCount.FindStringSubmatch(summary); {
	case m != nil:
		n, err := strconv.Atoi(m[1])
		if err == nil {
			res.PlanJSON = JSON(n)
		}
	case noChanges.MatchString(strings.TrimLeft(summary, "\n")):
		res.PlanJSON = JSON(0)
	}
	return res
}
