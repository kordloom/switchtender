package run

import "strings"

// Reversibility grades whether a run can be taken back, so a policy can demand more of a change
// that cannot be undone than of one that can. It is computed from the run and never stored, the way
// [Risk] is.
//
// Risk and reversibility are close enough to be confused and are not the same question. Risk asks
// how bad the outcome is if this goes wrong. Reversibility asks whether you get a second chance.
// Restarting a database fleet is high risk and fully reversible. Deleting last quarter's snapshots
// is quiet, unremarkable, and permanent.
type Reversibility struct {
	// Class is reversible, costly, or irreversible.
	Class string `json:"class"`
	// Reasons lists the signals that set the class, most to least severe.
	Reasons []string `json:"reasons,omitempty"`
}

// Reversibility classes, ordered by how hard the change is to take back.
const (
	// Reversible is a run that can be undone at no meaningful cost, which in practice means one
	// that changed nothing.
	Reversible = "reversible"
	// ReversibleCostly is a run that can be undone by doing more work: re-running against a prior
	// state, restoring from a backup, or bringing a service back up.
	ReversibleCostly = "costly"
	// Irreversible is a run whose effect cannot be undone from inside this product. The data is
	// gone, the volume is formatted, or the resource is destroyed.
	Irreversible = "irreversible"
)

// reversibilityRank orders the classes so a policy floor can compare them.
func reversibilityRank(class string) int {
	switch class {
	case Reversible:
		return 0
	case ReversibleCostly:
		return 1
	case Irreversible:
		return 2
	default:
		return -1
	}
}

// AssessReversibility grades whether r can be taken back.
//
// It is deliberately decisive rather than admitting an "unknown" class. An unknown would match no
// policy criterion, so every rule written to demand more of an irreversible change would quietly
// not apply to the runs nobody had classified, which is the failure mode the rule exists to
// prevent. Three classes with a defensible default beats four with a hole in it.
//
// The default for a real run is costly, not reversible. A run that changed something can nearly
// always be walked back by doing more work, and almost never for free, so costly is the honest
// middle. Reversible is reserved for a run that changed nothing at all.
func AssessReversibility(r *Run) Reversibility {
	return AssessReversibilityFrom(r, ReversibilityEvidence{})
}

// ReversibilityEvidence carries what a caller was able to gather, so a grade can be as true as the
// information available allows. Every field is optional and an absent one only ever leaves the
// grade less certain, never wrong in the safe direction.
type ReversibilityEvidence struct {
	// Playbook is the playbook's own text, when it could be read. It is what closes the gap for
	// Ansible, whose destructive work lives in a file rather than in a command.
	Playbook []byte
	// Hosts are the per host outcomes of a finished run. They answer the question no prediction
	// can: whether anything actually changed.
	Hosts []HostSummary
}

// AssessReversibilityFrom grades r using whatever evidence the caller could gather.
//
// The order matters and runs from strongest evidence to weakest. What actually happened beats what
// a file says will happen, which beats what a command line looks like.
func AssessReversibilityFrom(r *Run, ev ReversibilityEvidence) Reversibility {
	if r == nil {
		return Reversibility{Class: Reversible}
	}
	if r.DryRun {
		return Reversibility{Class: Reversible, Reasons: []string{"dry run, changes nothing to undo"}}
	}
	// Ansible is idempotent, and a finished run says how many tasks actually changed a host. None
	// means nothing happened, and nothing that happened can need undoing. This is the strongest
	// evidence there is, because it is the outcome rather than a prediction about it, and it is the
	// one case where a run that could have been destructive provably was not.
	if r.Status.Terminal() && len(ev.Hosts) > 0 {
		changed := 0
		for _, h := range ev.Hosts {
			changed += h.Changed
		}
		if changed == 0 {
			return Reversibility{Class: Reversible, Reasons: []string{
				"finished with no host reporting a change, so there is nothing to undo",
			}}
		}
	}

	cmd := strings.ToLower(r.Command)
	var reasons []string
	for _, marker := range permanentMarkers {
		if strings.Contains(cmd, marker) {
			reasons = append(reasons, "command contains "+marker+", which cannot be undone from here")
		}
	}
	if len(reasons) > 0 {
		return Reversibility{Class: Irreversible, Reasons: reasons}
	}
	// The playbook's own text, which is where an Ansible run keeps the work that a command line
	// would otherwise show. Only ever raises the grade, so a file that could not be read or an
	// include that was not followed leaves it where it was.
	if len(ev.Playbook) > 0 {
		signals, perr := ScanPlaybook(ev.Playbook)
		if perr == nil && len(signals.Permanent) > 0 {
			if signals.Deferred {
				signals.Permanent = append(signals.Permanent,
					"roles and includes were not followed, so there may be more")
			}
			return Reversibility{Class: Irreversible, Reasons: signals.Permanent}
		}
		if perr == nil {
			reasons := []string{"changes state, so undoing it means running something else",
				"the playbook was read and holds nothing this grades as permanent"}
			if signals.Deferred {
				reasons = append(reasons,
					"roles and includes were not followed, so this covers the playbook only")
			}
			return Reversibility{Class: ReversibleCostly, Reasons: reasons}
		}
	}
	if r.Command == "" {
		// Nothing to read. An Ansible run carries a playbook path, and what the playbook does is
		// inside a file this never opens: the content lives in a project, and may be fetched from
		// Vault or produced by a command at launch, so it is not available to a grade computed on
		// read. A playbook that drops every database grades exactly like one that restarts a
		// service.
		//
		// The class stays costly rather than climbing, because guessing from a file name would be
		// worse than saying nothing: it would put false confidence behind the word irreversible.
		// The reason carries the limit instead, so an approver knows what the grade rests on.
		return Reversibility{
			Class: ReversibleCostly,
			Reasons: []string{
				"changes state, so undoing it means running something else",
				"graded from what the run declares: the playbook's own contents were not examined",
			},
		}
	}
	return Reversibility{
		Class:   ReversibleCostly,
		Reasons: []string{"changes state, so undoing it means running something else"},
	}
}

// permanentMarkers are command fragments whose effect this product cannot undo, matched case
// insensitively.
//
// This is deliberately narrower than the risk grader's destructiveMarkers list, and the difference
// is the point. A reboot is destructive and fully reversible: the machine comes back. Dropping a
// table is destructive and permanent. Grading both the same way would make an irreversibility rule
// fire on every restart and get switched off.
var permanentMarkers = []string{
	"terraform destroy", "tofu destroy", "destroy -", "rm -rf", "rm -fr", "mkfs", "dd if=",
	"drop table", "drop database", "truncate ", "del /f", "remove-item", "format-volume",
}

// MeetsReversibilityFloor reports whether a run graded class is at least as hard to undo as floor.
//
// A floor names the easiest class a rule applies to, so a rule with a floor of costly covers costly
// and irreversible runs and leaves fully reversible ones alone. An unrecognized floor matches
// nothing rather than everything, so a typo in a policy file cannot silently widen a rule to the
// whole fleet.
func MeetsReversibilityFloor(class, floor string) bool {
	rank := reversibilityRank(floor)
	if rank < 0 {
		return false
	}
	return reversibilityRank(class) >= rank
}

// ValidReversibility reports whether class names a reversibility class this product grades.
func ValidReversibility(class string) bool {
	return reversibilityRank(class) >= 0
}
