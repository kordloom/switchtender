package run

import (
	"sort"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/util"
)

// The Ansible check_mode scanner, as the evidence its scans leave names it.
const (
	// CheckModeScanner names the reader that looks through a playbook, and everything it pulls in,
	// for work Ansible runs for real under --check.
	CheckModeScanner = "ansible-check-mode"
	// CheckModeScannerVersion is the revision of that reader's rules.
	CheckModeScannerVersion = 2
)

// DryRunScan returns what the playbook scan found, as the gate records it for an Ansible dry run.
//
// A play, block, task, role, or include that sets check_mode to anything but true runs for real
// under --check, so each one is a finding. Everything the scan could not read is recorded as well,
// since what was not read could set it too: the dry run is change free only when the scan read
// everything the playbook pulls in and found nothing.
//
// Unread content counts against the dry run here although it leaves a reversibility grade alone. A
// grade is a floor that only ever rises, so not reading a file costs it nothing. This
// classification exempts a run from approval, so not reading a file has to cost the exemption.
func (p *PlaybookSignals) DryRunScan() DryRunScan {
	if p == nil {
		return DryRunScan{}
	}
	return DryRunScan{
		Tool: ToolAnsible, Scanner: CheckModeScanner, Version: CheckModeScannerVersion,
		Source: p.Source, Inputs: append([]string(nil), p.Inputs...),
		Findings: append([]string(nil), p.Forced...), Unread: append([]string(nil), p.Unread...),
	}.Classified()
}

// UnreadPlaybookScan is what an Ansible dry run records when its playbook could not be read at
// all, saying why. Nothing proves such a playbook forces nothing, and the exemption a dry run earns
// rests on that proof, so the run is classified as incomplete rather than change free.
func UnreadPlaybookScan(playbook, why string) DryRunScan {
	return DryRunScan{
		Tool: ToolAnsible, Scanner: CheckModeScanner, Version: CheckModeScannerVersion,
		Unread: []string{label("playbook", playbook) + " (" + why + ")"},
	}.Classified()
}

// safeConnectionVars are the Ansible variables a dry run may set and stay change free. Each says who
// to connect as, where, or with what password, and none names a program. Every other name that
// begins with ansible_ is a finding: connection and interpreter variables such as
// ansible_ssh_common_args, ansible_python_interpreter, and ansible_shell_executable run a command
// on the controller under --check, and the connection plugins add new ones with every release, so a
// list of dangerous names would go stale where this list of harmless ones does not.
var safeConnectionVars = map[string]bool{
	"ansible_user": true, "ansible_ssh_user": true, "ansible_host": true, "ansible_ssh_host": true,
	"ansible_port": true, "ansible_ssh_port": true, "ansible_become": true,
	"ansible_become_user": true, "ansible_password": true, "ansible_ssh_pass": true,
	"ansible_ssh_password": true, "ansible_become_password": true, "ansible_become_pass": true,
}

// ConnectionVar reports whether a variable named name can change how Ansible connects or which
// program it runs, so setting it costs a dry run its change-free classification.
func ConnectionVar(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "ansible_") && !safeConnectionVars[lower]
}

// connectionVarWhy is the sentence a finding for a connection variable ends with.
const connectionVarWhy = "an ansible_ variable outside the connection settings the scan allows, " +
	"which can choose a program the controller runs during a dry run"

// WithExtraVars returns the scan with a finding for each extra var that sets a connection variable
// ConnectionVar names, or that holds template code at any depth, and classified again.
//
// Extra vars outrank every other variable and are templated on the controller, so a dry run whose
// playbook is clean still runs a program for real when its extra vars point the connection at one or
// carry a lookup. The finding names the variable, so the person deciding on the hold reads which.
func (s DryRunScan) WithExtraVars(vars map[string]any) DryRunScan {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := strconv.Quote(util.Clip(util.SafeText(k), maxScanLabel))
		if ConnectionVar(k) {
			s.Findings = append(s.Findings, "the extra var "+name+" sets "+connectionVarWhy)
		}
		if HoldsTemplate(vars[k]) {
			s.Findings = append(s.Findings, "the extra var "+name+" carries template code, which "+
				"Ansible renders on the controller during a dry run")
		}
	}
	return s.Classified()
}

// HoldsTemplate reports whether any string in value, a key of a nested mapping included, holds a
// marker that starts template code.
func HoldsTemplate(value any) bool {
	switch v := value.(type) {
	case string:
		return inventory.HasTemplateMarker(v)
	case map[string]any:
		for k, item := range v {
			if inventory.HasTemplateMarker(k) || HoldsTemplate(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if HoldsTemplate(item) {
				return true
			}
		}
	}
	return false
}
