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

	// The same text the risk grader reads, for the same reason and with more at stake. A variable
	// is string material a playbook splices into what it executes, so a destructive command riding
	// in -e was graded fully reversible while the identical text on the command line was graded
	// permanent. Reversibility is the grade an approval policy is told to hold on, so the grader
	// that misses it is the one whose miss costs something.
	var vars strings.Builder
	writeVarText(&vars, r.ExtraVars, maxVarScanDepth)
	cmd := strings.ToLower(r.Command + " " + r.Playbook + vars.String())
	var reasons []string
	reasons = append(reasons, permanentCommandFindings(cmd)...)
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
	// Infrastructure teardown by subcommand. The -destroy flag on apply is a token predicate
	// below, because as a substring it also matched "terraform plan -destroy", which is the
	// read-only preview of a destroy and the exact command an operator runs to be careful.
	"terraform destroy", "tofu destroy",
	// Filesystem and block device.
	"mkfs", "dd if=", "wipefs", "shred ", "blkdiscard", "sgdisk",
	"lvremove", "vgremove", "pvremove", "remove-item", "format-volume",
	// Databases. Dropping a schema loses what it held exactly as dropping a table does; it was
	// absent here while the narrower two were present, so an estate's worst statement graded safe.
	"drop table", "drop database", "drop schema", "drop keyspace", "truncate ", "flushall",
	// Cloud object and instance deletion. An operator reaches for these far more often than for
	// mkfs, and none of them was recognized at all.
	"aws s3 rb", "s3 rm ", "delete-bucket", "delete-db-instance", "delete-db-cluster",
	"terminate-instances", "delete-table", "instances delete", "group delete",
	// Kubernetes objects whose removal takes the data with them. A deployment is a definition and
	// comes back; a namespace and a volume claim do not.
	"kubectl delete namespace", "kubectl delete ns ", "kubectl delete pvc", "kubectl delete pv ",
	"helm uninstall", "helm delete",
}

// permanentCommandFindings returns every reason cmd holds work this product cannot undo. cmd must
// already be lowercased. It is the one scanner all three graders share: reversibility, risk, and
// the playbook scan each used to keep their own copy of this judgment, and the copies drifted in
// whichever direction was worst for the grader that held them.
func permanentCommandFindings(cmd string) []string {
	var reasons []string
	for _, marker := range permanentMarkers {
		if strings.Contains(cmd, marker) {
			reasons = append(reasons, "command contains "+strings.TrimSpace(marker)+
				", which cannot be undone from here")
		}
	}
	for _, segment := range commandSegments(cmd) {
		reasons = append(reasons, segmentFindings(segment)...)
	}
	return reasons
}

// commandSegments splits a command line into the token lists of its simple commands, so a
// predicate reads one command at a time. Splitting on the shell's sequencing characters is
// deliberate: a destructive command hidden behind && or | is still a destructive command.
func commandSegments(cmd string) [][]string {
	parts := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ';', '|', '&', '(', ')', '\n', '`':
			return true
		}
		return false
	})
	segments := make([][]string, 0, len(parts))
	for _, part := range parts {
		if fields := strings.Fields(part); len(fields) > 0 {
			segments = append(segments, fields)
		}
	}
	return segments
}

// segmentFindings grades one simple command by its tokens: the judgments substring matching gets
// wrong in both directions. /bin/rm and sudo rm are rm; "predelete-hook" is not -delete; and
// "terraform plan -destroy" is somebody previewing a destroy, not running one.
func segmentFindings(fields []string) []string {
	var reasons []string
	if removesRecursivelyByForce(fields) {
		reasons = append(reasons,
			"command removes recursively and forcibly, which cannot be undone from here")
	}
	if hasCommand(fields, "find") && hasToken(fields, "-delete") {
		reasons = append(reasons,
			"command runs find -delete, which removes what it matched and cannot be undone from here")
	}
	if hasCommand(fields, "rsync") && hasTokenPrefix(fields, "--delete") {
		reasons = append(reasons,
			"command runs rsync --delete, which removes what the source lacks and cannot be undone from here")
	}
	if (hasCommand(fields, "terraform") || hasCommand(fields, "tofu")) &&
		hasToken(fields, "apply") &&
		(hasToken(fields, "-destroy") || hasTokenPrefix(fields, "-destroy=") ||
			hasToken(fields, "--destroy")) {
		reasons = append(reasons,
			"command applies a destroy plan, which cannot be undone from here")
	}
	if hasCommand(fields, "del") && (hasToken(fields, "/f") || hasToken(fields, "/q")) {
		reasons = append(reasons,
			"command runs del with a force flag, which cannot be undone from here")
	}
	return reasons
}

// removesRecursivelyByForce reports whether this command runs rm both recursively and forcibly,
// however the flags are spelled or split. Any token whose base name is rm counts as the command,
// so /bin/rm, busybox rm, sudo rm, and xargs rm are all seen; flag reading stops at --, after
// which everything is a path.
func removesRecursivelyByForce(fields []string) bool {
	for i, field := range fields {
		if baseName(field) != "rm" {
			continue
		}
		var recursive, force bool
		for _, arg := range fields[i+1:] {
			if arg == "--" {
				break
			}
			switch {
			case arg == "--recursive":
				recursive = true
			case arg == "--force":
				force = true
			case strings.HasPrefix(arg, "--"):
			case strings.HasPrefix(arg, "-") && len(arg) > 1:
				recursive = recursive || strings.ContainsAny(arg, "rR")
				force = force || strings.Contains(arg, "f")
			}
		}
		if recursive && force {
			return true
		}
	}
	return false
}

// baseName strips any path prefix from a token, so /bin/rm and rm read the same.
func baseName(token string) string {
	if i := strings.LastIndexByte(token, '/'); i >= 0 {
		return token[i+1:]
	}
	return token
}

// hasCommand reports whether any token's base name is name, which covers the command itself and
// the common prefixes that wrap one: sudo, env assignments, busybox, xargs.
func hasCommand(fields []string, name string) bool {
	for _, f := range fields {
		if baseName(f) == name {
			return true
		}
	}
	return false
}

// hasToken reports whether any token equals want exactly.
func hasToken(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// hasTokenPrefix reports whether any token starts with want.
func hasTokenPrefix(fields []string, want string) bool {
	for _, f := range fields {
		if strings.HasPrefix(f, want) {
			return true
		}
	}
	return false
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
