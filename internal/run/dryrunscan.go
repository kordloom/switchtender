package run

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Classifications a dry-run scan records.
const (
	// DryRunChangeFree means the scan read everything the dry run executes and found nothing in it
	// that runs work for real.
	DryRunChangeFree = "change_free"
	// DryRunNotChangeFree means the scan found something that runs work for real during the dry run,
	// such as an Ansible task that sets check_mode to false or a Terraform external data source.
	DryRunNotChangeFree = "not_change_free"
	// DryRunIncomplete means the scan found nothing in what it read but could not read everything
	// the dry run executes, so it cannot classify the dry run as change free either.
	DryRunIncomplete = "incomplete"
)

// DryRunScan is one read the gate made of what a dry run executes, before it judged the run.
//
// A dry run is the tool's no-change mode, and that mode is a promise about the tool rather than
// about what it is given. Ansible runs a task that sets check_mode to false for real under
// --check, and Terraform runs the program an external data source names while it plans. A rule
// that exempted every dry run on its flag therefore waved real work through as long as it was
// asked for as a preview. The gate reads what the dry run executes instead, with a scanner per
// tool, and records what it read here. A finding means the dry run cannot be classified as change
// free, not that it has side effects: the scan cannot tell, so it does not exempt.
//
// The record is the same for every tool, so every judgment that once read DryRun as "nothing
// happens" reads one thing, and every place that shows the judgment shows the same evidence.
type DryRunScan struct {
	// Step names the pipeline step whose dry run was read, empty for the run's own.
	Step string `json:"step,omitempty"`
	// Tool is the tool whose input was read: ansible, terraform, or opentofu.
	Tool string `json:"tool"`
	// Scanner names the reader that made the classification.
	Scanner string `json:"scanner"`
	// Version is the revision of the scanner's rules, so a classification can be traced to the
	// logic that made it after those rules change.
	Version int `json:"version"`
	// Source says which version of the files was read, such as the commit, and is empty when the
	// files have only the one version.
	Source string `json:"source,omitempty"`
	// Inputs lists the files the scan read, sorted, relative to the tree it read them from.
	Inputs []string `json:"inputs,omitempty"`
	// Findings lists what runs work for real during the dry run, each naming where it is declared.
	Findings []string `json:"findings,omitempty"`
	// Unread lists what the dry run executes that the scan could not read, each with why. Anything
	// here could hold a finding, so it costs the dry run its classification as well.
	Unread []string `json:"unread,omitempty"`
	// Classification is change_free, not_change_free, or incomplete, as Classified sets it.
	Classification string `json:"classification"`
	// Fetch records the module download the gate ran so the scan could read the registry and
	// remote modules a Terraform or OpenTofu configuration calls, nil when it ran none.
	Fetch *ModuleFetch `json:"fetch,omitempty"`
}

// ModuleFetch is the module download the gate ran before a scan: the tool's get, in a private copy
// of the configuration, with the credentials and network the plan itself would use. It downloads
// modules only. It installs no provider and runs no program a configuration names.
type ModuleFetch struct {
	// Command is what ran, such as "terraform get".
	Command string `json:"command"`
	// ExitStatus is the command's exit status, or -1 when it could not start or was stopped.
	ExitStatus int `json:"exit_status"`
	// Error says why the download did not complete, empty when it did.
	Error string `json:"error,omitempty"`
	// ModulesDigest is the digest of the module tree the download installed and the scan read: the
	// path and content of every file under .terraform/modules, version control metadata aside. The
	// run is held to it, so it executes exactly the modules the gate read or does not execute.
	ModulesDigest string `json:"modules_digest,omitempty"`
}

// Fetched reports whether the download completed, so the scan read what it installed.
func (f *ModuleFetch) Fetched() bool {
	return f != nil && f.ExitStatus == 0 && f.Error == ""
}

// Summary says in a phrase whether the download completed, with its exit status, for a record a
// person reads beside the scan it fed. It is empty when no download ran.
func (f *ModuleFetch) Summary() string {
	switch {
	case f == nil:
		return ""
	case f.Fetched():
		return fmt.Sprintf("modules downloaded first by the gate's %s (exit status 0)", f.Command)
	}
	return fmt.Sprintf("the gate's %s did not download its modules (exit status %d)", f.Command,
		f.ExitStatus)
}

// Classified returns a copy of s whose Classification is the one its findings and unread entries
// give: not_change_free when anything was found, incomplete when nothing was found but something
// could not be read, and change_free otherwise.
func (s DryRunScan) Classified() DryRunScan {
	switch {
	case len(s.Findings) > 0:
		s.Classification = DryRunNotChangeFree
	case len(s.Unread) > 0:
		s.Classification = DryRunIncomplete
	default:
		s.Classification = DryRunChangeFree
	}
	return s
}

// ChangeFree reports whether the scan read everything the dry run executes and found nothing that
// runs work for real. It reads the findings and unread entries rather than the recorded
// classification, so a record whose label disagrees with its contents is judged by its contents.
func (s DryRunScan) ChangeFree() bool {
	return len(s.Findings) == 0 && len(s.Unread) == 0
}

// Entries returns what keeps the dry run from being change free, as sentences: each finding, then
// each thing the scan could not read, saying what it may therefore do.
func (s DryRunScan) Entries() []string {
	out := make([]string, 0, len(s.Findings)+len(s.Unread))
	out = append(out, s.Findings...)
	for _, unread := range s.Unread {
		out = append(out, "could not read "+unread+", "+wordsFor(s.Tool).unread)
	}
	return out
}

// Clone returns a copy of s that shares no slice or record with it.
func (s DryRunScan) Clone() DryRunScan {
	s.Inputs = append([]string(nil), s.Inputs...)
	s.Findings = append([]string(nil), s.Findings...)
	s.Unread = append([]string(nil), s.Unread...)
	if s.Fetch != nil {
		fetch := *s.Fetch
		s.Fetch = &fetch
	}
	return s
}

// dryRunWords is how the scans of one tool word what they report.
type dryRunWords struct {
	// lead opens the one-line summary of a dry run that is not change free.
	lead string
	// unread completes the sentence about something the scan could not read.
	unread string
}

// wordsFor returns the wording for scans of tool. A tool with no scanner of its own reads in the
// most general terms, which no scan of it should ever need.
func wordsFor(tool string) dryRunWords {
	switch NormalizeTool(tool) {
	case ToolAnsible:
		return dryRunWords{lead: "dry run that still runs work for real under check mode",
			unread: "so it may set check_mode to false"}
	case ToolTerraform, ToolOpenTofu:
		return dryRunWords{lead: "plan that may run a program while it plans",
			unread: "so it may run a program during plan"}
	}
	return dryRunWords{lead: "dry run that may run work for real",
		unread: "so it may run work for real"}
}

// ChangeFree reports whether r is a dry run that changes nothing: a dry run every scan the gate
// made of it read in full and found clean. A dry run the gate never scanned keeps the meaning its
// flag gives it, which is what a run of a tool with no scanner gets.
//
// Every judgment that once read DryRun as "nothing happens" reads this instead, so a dry run the
// gate found running work for real is matched, graded, and shown as the change it may be.
func (r *Run) ChangeFree() bool {
	if r == nil || !r.DryRun {
		return false
	}
	// A dry run the gate never scanned is proven to change nothing only for a built-in tool, whose
	// no-change mode is a fixed syntax, compile, or parse check that executes nothing: bash -n,
	// py_compile, go vet, a PowerShell parse. Ansible, Terraform, and OpenTofu always carry a scan
	// here, so an empty scan set means one of those inert built-ins, or a tool a plugin added. A
	// plugin tool defines its own dry run and runs it on the host as whatever the plugin coded, so
	// nothing proves it changes nothing and it is not read as a preview.
	if len(r.DryRunScans) == 0 {
		return IsBuiltinTool(r.Tool)
	}
	return ScansChangeFree(r.DryRunScans)
}

// ScansChangeFree reports whether every scan in scans read in full and found nothing.
func ScansChangeFree(scans []DryRunScan) bool {
	for _, s := range scans {
		if !s.ChangeFree() {
			return false
		}
	}
	return true
}

// ModulesDigest returns the digest of the module tree the gate downloaded and read before it judged
// r, empty when it downloaded none. Execution holds r to it.
func (r *Run) ModulesDigest() string {
	if r == nil {
		return ""
	}
	for _, s := range r.DryRunScans {
		if s.Fetch.Fetched() && s.Fetch.ModulesDigest != "" {
			return s.Fetch.ModulesDigest
		}
	}
	return ""
}

// ModulesDigests returns the digest of every module tree the gate downloaded and read before it
// judged r, in scan order, each naming the pipeline step it belongs to when it has one. It is what
// an approval binds, so a decision covers the exact modules every step will run.
func (r *Run) ModulesDigests() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, s := range r.DryRunScans {
		if !s.Fetch.Fetched() || s.Fetch.ModulesDigest == "" {
			continue
		}
		if s.Step != "" {
			out = append(out, StepPrefix(s.Step)+s.Fetch.ModulesDigest)
			continue
		}
		out = append(out, s.Fetch.ModulesDigest)
	}
	return out
}

// DryRunFindings returns why r's dry run may not be change free, one sentence each. A pipeline's
// entries name the step each belongs to. It is empty when every scan was clean or none was made.
func (r *Run) DryRunFindings() []string {
	if r == nil {
		return nil
	}
	return ScanEntries(r.DryRunScans)
}

// ScanEntries returns the entries of every scan in scans, each naming its step when it has one.
func ScanEntries(scans []DryRunScan) []string {
	var out []string
	for _, s := range scans {
		prefix := ""
		if s.Step != "" {
			prefix = StepPrefix(s.Step)
		}
		for _, entry := range s.Entries() {
			out = append(out, prefix+entry)
		}
	}
	return out
}

// StepPrefix is how an entry from a pipeline step's scan names the step it belongs to.
func StepPrefix(step string) string {
	return fmt.Sprintf("step %q: ", step)
}

// NotChangeFreeSummary says, in one sentence fragment, why a dry run may run work for real, or
// returns the empty string when the gate recorded nothing. The graders and the evidence share it,
// so the approver reads the same words wherever the run is shown.
func (r *Run) NotChangeFreeSummary() string {
	if r == nil {
		return ""
	}
	entries := r.DryRunFindings()
	if len(entries) == 0 {
		return ""
	}
	lead := wordsFor("").lead
	for _, s := range r.DryRunScans {
		if !s.ChangeFree() {
			lead = wordsFor(s.Tool).lead
			break
		}
	}
	if n := len(entries); n > 1 {
		return fmt.Sprintf("%s: %s, and %d more", lead, entries[0], n-1)
	}
	return lead + ": " + entries[0]
}

// ScansForStep returns copies of scans labeled with the pipeline step they read, which is how a
// pipeline records its steps' scans as its own.
func ScansForStep(step string, scans []DryRunScan) []DryRunScan {
	out := make([]DryRunScan, 0, len(scans))
	for _, s := range scans {
		c := s.Clone()
		c.Step = step
		out = append(out, c)
	}
	return out
}

// StepDryRunScans returns the scans r records for the named pipeline step, with the step label
// cleared, which is how a step run takes back its own share of its pipeline's record.
func (r *Run) StepDryRunScans(step string) []DryRunScan {
	if r == nil {
		return nil
	}
	var out []DryRunScan
	for _, s := range r.DryRunScans {
		if s.Step == step {
			c := s.Clone()
			c.Step = ""
			out = append(out, c)
		}
	}
	return out
}

// ScansColumn encodes a run's dry-run scans for a text column, empty for none.
func ScansColumn(scans []DryRunScan) string {
	if len(scans) == 0 {
		return ""
	}
	b, err := json.Marshal(scans)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseScansColumn decodes stored dry-run scans. An empty column is a run the gate never scanned. A
// column that does not decode is reported rather than read as empty, because the scans are
// evidence and a judgment rests on them: read as absent, a dry run the gate found running work for
// real would read back as one that changes nothing.
func ParseScansColumn(s string) ([]DryRunScan, error) {
	if s == "" {
		return nil, nil
	}
	var out []DryRunScan
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse dry-run scans: %w", err)
	}
	return out, nil
}

// cloneScans returns a deep copy of scans, nil for none.
func cloneScans(scans []DryRunScan) []DryRunScan {
	if scans == nil {
		return nil
	}
	out := make([]DryRunScan, len(scans))
	for i, s := range scans {
		out[i] = s.Clone()
	}
	return out
}

// toolLabel names a tool as the hold note says it.
func toolLabel(tool string) string {
	switch NormalizeTool(tool) {
	case ToolOpenTofu:
		return "OpenTofu"
	case ToolTerraform:
		return "Terraform"
	}
	return "Ansible"
}

// ExemptionHoldNote is the hold message for a dry run held only because the gate found it not
// change free: the rule holds every run it matches except a dry run that changes nothing, and this
// dry run was not shown to change nothing. It names what was found, or what could not be read, and
// the two clean fixes. A rule written in Rego has no exclude_dry_run, so its second fix is to stop
// exempting dry runs in its module. It returns the empty string for a run with nothing recorded.
//
// The fixes are the only two, deliberately. There is no per-task or per-resource override: a mark
// saying "this one is safe" is a claim the gate cannot check, written by whoever wants the run to
// go through, which is the bypass the scan exists to close.
func ExemptionHoldNote(r *Run, rule string, rego bool) string {
	drop := "drop exclude_dry_run from " + quoteRule(rule)
	if rego {
		drop = "stop exempting dry runs in the module of " + quoteRule(rule)
	}
	return holdNote(r, "This dry run was not shown to change nothing, so "+quoteRule(rule)+
		" does not exempt it", drop)
}

// holdNote builds a dry run's hold message: lead, the first thing its scans found, and the two
// clean fixes, the second of which is second.
func holdNote(r *Run, lead, second string) string {
	entries := r.DryRunFindings()
	if len(entries) == 0 {
		return ""
	}
	first := entries[0]
	if n := len(entries); n > 1 {
		first = fmt.Sprintf("%s (and %d more)", first, n-1)
	}
	drop := second
	tool, incomplete := heldScan(r)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s. ", lead, first)
	switch {
	case tool == ToolAnsible && incomplete:
		fmt.Fprintf(&b, "Two clean fixes: make what the playbook pulls in readable to the gate, "+
			"such as a role committed to the project or installed from its requirements file, or %s.",
			drop)
	case tool == ToolAnsible:
		fmt.Fprintf(&b, "Two clean fixes: rework the task so check mode is safe, or %s.", drop)
	case incomplete:
		fmt.Fprintf(&b, "Two clean fixes: make every module the configuration calls readable to "+
			"the gate, which downloads registry and remote modules with the run's own credentials "+
			"and reads local paths in the project, or %s.", drop)
	default:
		fmt.Fprintf(&b, "Two clean fixes: replace the external data source with one that runs no "+
			"program while %s plans, or %s.", toolLabel(tool), drop)
	}
	return b.String()
}

// heldScan returns the tool of the first scan that is not change free, and whether that scan found
// nothing and was only incomplete.
func heldScan(r *Run) (string, bool) {
	for _, s := range r.DryRunScans {
		if !s.ChangeFree() {
			return NormalizeTool(s.Tool), len(s.Findings) == 0
		}
	}
	return NormalizeTool(r.Tool), false
}

// quoteRule renders a rule's label as a hold note names it.
func quoteRule(rule string) string {
	if rule == "" {
		return "the rule"
	}
	return fmt.Sprintf("%q", rule)
}
