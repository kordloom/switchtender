package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// dryRunScanner is the part of the dispatcher a review asks, before it plans, what a dry run of
// the plan would execute, read the way the gate reads it.
type dryRunScanner interface {
	// DryRunScansAt returns the gate's scans of a dry run of r, nil when the question does not apply.
	DryRunScansAt(ctx context.Context, r *run.Run) []run.DryRunScan
}

// maxRefusalForced bounds how many entries a refusal names before it counts the rest.
const maxRefusalForced = 3

// reviewProbe builds the run a pull request's plan would be, from the template and the plan's
// options, so the gate can be asked about it before any run exists.
func reviewProbe(t *template.Template, opts []run.SubmitOption) *run.Run {
	probe := &run.Run{Playbook: t.Playbook, Inventory: t.Inventory}
	run.ApplyOptions(probe, opts)
	return probe
}

// precheckRefusal refuses a pull request's plan that the gate cannot classify as change free, and
// reports whether it did, with what the forge is told when it does.
//
// A plan is a promise that nothing changes. An Ansible playbook that sets check_mode to false runs
// that work for real under --check, and a Terraform or OpenTofu configuration with an external data
// source runs its program while it plans, so planning either would execute code from a branch
// nobody merged, with the template's credentials. So would whatever the gate could not read. The
// plan is refused before any run exists, recorded, and the pull request is told what the scan found
// or could not read, with an error status rather than a plan. This is the same scan, and the same
// rule, a manual or integration-triggered plan meets at the gate.
func precheckRefusal(ctx context.Context, d reviewHookDeps, tg *trigger.Trigger, ev *review.Event,
	probe *run.Run, hookPath string) (hookAnswer, bool) {
	scanner, ok := d.submitter.(dryRunScanner)
	if !ok {
		return hookAnswer{}, false
	}
	scans := scanner.DryRunScansAt(ctx, probe)
	if run.ScansChangeFree(scans) {
		return hookAnswer{}, false
	}
	receipt, err := appendHookEntry(ctx, d.audits, tg, hookPath+"/refused")
	if err != nil {
		d.log.Error("server: record review webhook: " + err.Error())
		return hookAnswer{status: http.StatusServiceUnavailable,
			message: "refused: the webhook could not be recorded in the audit trail"}, true
	}
	message, reason := dryRunRefusal(scans)
	d.reviews.Refuse(tg, ev, message, receipt)
	return hookAnswer{status: http.StatusAccepted, receipt: receipt,
		body: map[string]string{"trigger": tg.ID, "refused": reason}}, true
}

// dryRunRefusal is what a pull request is told when its plan is refused because the gate did not
// find it change free, and the short reason the webhook's answer gives. It is worded for the tool
// whose scan stopped it, and names what was found, or what could not be read, and what to change.
func dryRunRefusal(scans []run.DryRunScan) (string, string) {
	var held run.DryRunScan
	for _, s := range scans {
		if !s.ChangeFree() {
			held = s
			break
		}
	}
	entries := run.ScanEntries(scans)
	shown := entries[:min(len(entries), maxRefusalForced)]
	list := strings.Join(shown, "; ")
	if rest := len(entries) - len(shown); rest > 0 {
		list += fmt.Sprintf("; and %d more", rest)
	}
	terraform := held.Tool == run.ToolTerraform || held.Tool == run.ToolOpenTofu
	switch {
	case terraform && len(held.Findings) == 0:
		return "This configuration could not be read in full before planning, so nothing shows a " +
				"plan of it would run no program: " + list + ". Before it plans, the gate downloads " +
				"the registry and remote modules a configuration calls, with the template's own " +
				"credentials and where the plan runs, and reads them. Make every module this one " +
				"calls reachable that way or committed to the repository, or review it with a " +
				"template whose configuration the gate can read in full.",
			"the configuration could not be read in full"
	case terraform:
		return "This configuration runs a program while it plans, so a plan of it would execute " +
				"code from a branch nobody merged: " + list + ". Replace the external data source " +
				"with one that runs no program, or review it with a template whose configuration " +
				"declares none.",
			"the configuration runs a program while it plans"
	case len(held.Findings) == 0:
		return "This playbook could not be read in full before planning, so nothing shows a plan " +
				"of it would leave hosts unchanged: " + list + ". Make what it pulls in readable to " +
				"the gate, such as a role committed to the repository, or review it with a template " +
				"whose playbook the gate can read in full.",
			"the playbook could not be read in full"
	}
	return "This playbook runs work for real even under check mode, so a plan of it would change " +
			"hosts from a branch nobody merged: " + list + ". Remove check_mode: false from what " +
			"runs in this plan, or review it with a template whose playbook forces nothing.",
		"the playbook runs tasks for real under check mode"
}
