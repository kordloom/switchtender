package run

// The Ansible check_mode scanner, as the evidence its scans leave names it.
const (
	// CheckModeScanner names the reader that looks through a playbook, and everything it pulls in,
	// for work Ansible runs for real under --check.
	CheckModeScanner = "ansible-check-mode"
	// CheckModeScannerVersion is the revision of that reader's rules.
	CheckModeScannerVersion = 1
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
