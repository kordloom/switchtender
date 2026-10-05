package dispatch

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/tfscan"
)

// dryRunScan returns the gate's scan of what the dry run r executes, and whether r is a run the
// question applies to: an Ansible dry run, read through the scan of its playbook, or a Terraform or
// OpenTofu plan, read through its configuration.
//
// A playbook or configuration that could not be read is an answer too. Nothing proves it runs
// nothing for real, and the exemption a dry run earns rests on that proof, so the run is classified
// as incomplete rather than change free. A pipeline's coordinator executes nothing of its own,
// since its playbook field holds the pipeline's name, so its steps are scanned instead.
func dryRunScan(r *run.Run, pb *run.PlaybookSignals, unread string, checkouts CheckoutReader,
	plans planScanner) (run.DryRunScan, bool) {
	if r == nil || !r.DryRun || r.Kind == run.KindPipeline {
		return run.DryRunScan{}, false
	}
	switch run.NormalizeTool(r.Tool) {
	case run.ToolAnsible:
		if r.Playbook == "" {
			return run.DryRunScan{}, false
		}
		if pb == nil {
			return run.UnreadPlaybookScan(r.Playbook, unread), true
		}
		return pb.DryRunScan(), true
	case run.ToolTerraform, run.ToolOpenTofu:
		if r.Command == "" {
			return run.DryRunScan{}, false
		}
		return plans(r, checkouts), true
	}
	return run.DryRunScan{}, false
}

// scanConfiguration reads the Terraform or OpenTofu configuration r plans, and every module it
// calls, from wherever it lives: the project at the commit that decides what the run executes, or
// the working directory on this server for a run naming one by path. It downloads nothing, so a
// registry or remote module is read only where a download already put it.
func scanConfiguration(r *run.Run, checkouts CheckoutReader) run.DryRunScan {
	if r.ProjectID == "" {
		return scanLocalConfiguration(r)
	}
	if checkouts == nil {
		return unreadConfiguration(r, noCheckout)
	}
	commit, which := commitToRead(r)
	var out run.DryRunScan
	read := false
	err := checkouts.ReadCommit(r.ProjectID, commit, func(fsys fs.FS, sha string) error {
		out = tfscan.Scan(fsys, projectDir(r.Command), tfscan.Options{Tool: r.Tool,
			Place: "the project"})
		out.Source = readAt(sha, which)
		read = true
		return nil
	})
	if err != nil || !read {
		return unreadConfiguration(r, "the project's checkout could not be read")
	}
	return out
}

// scanLocalConfiguration reads a configuration named by a working directory on this server. The
// directory bounds what the scan reads, so a module outside it is listed as unread rather than
// followed.
func scanLocalConfiguration(r *run.Run) run.DryRunScan {
	root, _, err := openLocal(r.Command, false)
	if err != nil {
		return unreadConfiguration(r, unreadWhy(err, "not on this server"))
	}
	defer func() { _ = root.Close() }()
	return tfscan.Scan(root.FS(), ".", tfscan.Options{Tool: r.Tool, Place: "the working directory"})
}

// unreadConfiguration is the scan a plan records when its configuration could not be read at all.
func unreadConfiguration(r *run.Run, why string) run.DryRunScan {
	tool := run.ToolTerraform
	if run.NormalizeTool(r.Tool) == run.ToolOpenTofu {
		tool = run.ToolOpenTofu
	}
	return run.DryRunScan{
		Tool: tool, Scanner: tfscan.Scanner, Version: tfscan.Version,
		Unread: []string{"the configuration in " + strconv.Quote(r.Command) + " (" + why + ")"},
	}.Classified()
}

// projectDir returns a project run's working directory as a path inside the project, the way the
// runner joins it to the checkout: a leading slash names the checkout's root rather than the
// machine's.
func projectDir(command string) string {
	dir := strings.TrimPrefix(filepath.ToSlash(filepath.Join("/", command)), "/")
	if dir == "" {
		return "."
	}
	return path.Clean(dir)
}

// exemptionHoldNote returns the hold message for gr when the rule that held it, held, holds it only
// because the gate did not find the dry run change free: the same rules, judging the same run as a
// dry run that changes nothing, would have let it through. It is empty otherwise, because a dry run
// a rule holds either way was not held for what its scan found, and naming fixes that release
// nothing would send the operator after the wrong thing.
func exemptionHoldNote(policies []*policy.Policy, gr *run.Run, held *policy.Policy) string {
	if gr == nil || !gr.DryRun || gr.ChangeFree() || held == nil {
		return ""
	}
	if policy.Requiring(policies, asChangeFree(gr)) != nil {
		return ""
	}
	return run.ExemptionHoldNote(gr, ruleOf(policies, held), held.Rego != nil)
}

// asChangeFree returns a copy of the graded run gr judged as the dry run it would be had its scans
// found nothing: the same graded copy with the scans set aside, and the reversibility grade left to
// be computed again, since the one gr carries was raised by what the scans found.
func asChangeFree(gr *run.Run) *run.Run {
	clean := *gr
	clean.DryRunScans = nil
	clean.Reversibility = nil
	return &clean
}

// ruleOf returns the label of the stored policy a decision came from. A Rego verdict is a copy
// labeled with its reasons, so it is named by the policy that carries the same program.
func ruleOf(policies []*policy.Policy, decided *policy.Policy) string {
	if decided.Rego != nil {
		for _, p := range policies {
			if p.Rego == decided.Rego {
				return p.Label()
			}
		}
	}
	return decided.Label()
}

// DryRunScansAt reads what a dry run of r would execute, the way the gate reads it: from the
// project at the commit r is pinned to, after fetching the ref r names, or from this server for a
// run naming its playbook or working directory by path. A playbook or configuration that cannot be
// read is reported as incomplete, since nothing proves it runs nothing for real. It returns nil for
// a run the question does not apply to, such as one of a tool with no scanner, and never changes r.
//
// It is what a pull request review asks before it plans: a plan is a promise that nothing changes,
// and a playbook that forces real tasks under check mode, or a configuration that runs a program
// while it plans, would break it on a branch nobody merged.
func (d *Dispatcher) DryRunScansAt(ctx context.Context, r *run.Run) []run.DryRunScan {
	if r == nil {
		return nil
	}
	probe := *r
	probe.DryRun = true
	// The probe stands for the run about to be submitted, and a module download for it opens
	// credentials and private files labeled by a run id, so it carries one of its own.
	if probe.ID == "" {
		probe.ID = run.NewID()
	}
	if scannable(&probe) {
		d.fetchRefForGate(ctx, &probe)
	}
	checkouts := d.checkoutReader()
	pb, unread := scanRunPlaybook(&probe, checkouts)
	scan, ok := dryRunScan(&probe, pb, unread, checkouts, d.precheckPlan)
	if !ok {
		return nil
	}
	return []run.DryRunScan{scan}
}

// scannable reports whether the gate reads r's input to judge it: an Ansible run's playbook, or a
// Terraform or OpenTofu run's working directory.
func scannable(r *run.Run) bool {
	switch run.NormalizeTool(r.Tool) {
	case run.ToolAnsible:
		return r.Playbook != ""
	case run.ToolTerraform, run.ToolOpenTofu:
		return r.Command != ""
	}
	return false
}
