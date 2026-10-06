package review

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// Phases of a review a report describes.
const (
	// PhaseRunning means the plan is queued or executing.
	PhaseRunning = "running"
	// PhaseHeld means a rule holds the plan itself for approval before it may execute.
	PhaseHeld = "held"
	// PhaseSucceeded means the plan finished.
	PhaseSucceeded = "succeeded"
	// PhaseFailed means the plan failed, was interrupted, or was rejected by an approver.
	PhaseFailed = "failed"
	// PhaseCanceled means the plan was canceled before it finished.
	PhaseCanceled = "canceled"
	// PhaseRefused means no plan was run: the pull request comes from a fork, or a rule refused
	// the plan outright.
	PhaseRefused = "refused"
	// PhaseReplied is a comment command's reply, posted as a new comment.
	PhaseReplied = "replied"
)

// Excerpt bounds. A forge caps a comment's size, GitHub at 65536 characters, and a plan worth
// reading in a comment is far shorter than that, so the excerpt stops well before and the comment
// links to the full log instead.
const (
	// maxExcerptLines is the most plan lines a comment carries.
	maxExcerptLines = 250
	// maxExcerptBytes is the most plan bytes a comment carries.
	maxExcerptBytes = 30000
	// maxCommentBytes is the hard cap on a whole comment body.
	maxCommentBytes = 60000
	// maxDescription is GitHub's limit on a commit status description.
	maxDescription = 140
)

// What a pull request from a fork is told when its trigger does not plan forks. It is a commit
// status alone, never a comment, so opening pull requests from forks cannot turn the operator's
// token into a way to write comments. The link explains why and how a maintainer allows it.
const (
	// ForkStatus is the commit status description.
	ForkStatus = "Not planned: this repository doesn't run plans for pull requests from forks"
	// ForkDocsURL is where the status links.
	ForkDocsURL = "https://switchtender.com/docs/pull-request-review#pull-requests-from-forks"
)

// What a pull request is told when its plan was skipped because the template's inventory matched no
// hosts. Like a fork's refusal it is a commit status alone: nothing went wrong with the pull
// request, and the inventory, not the code, is what to look at.
const (
	// NoHostsStatus is the commit status description.
	NoHostsStatus = "Not planned: the inventory matched no hosts"
	// NoHostsDocsURL is where the status links.
	NoHostsDocsURL = "https://switchtender.com/docs/pull-request-review#when-the-inventory-matches-no-hosts"
)

// Report is everything one review comment and its commit status say.
type Report struct {
	// TemplateID identifies the template, and with it the one comment this report replaces.
	TemplateID string
	// TemplateName labels the template for a reader.
	TemplateName string
	// Phase is running, held, succeeded, failed, canceled, or refused.
	Phase string
	// Reason explains a refused or failed phase in one sentence, already masked.
	Reason string
	// HeldBy names the rule holding the plan itself, for the held phase.
	HeldBy string
	// RunID is the plan run, empty when none was created.
	RunID string
	// RunURL links the plan run, empty when the server has no public address.
	RunURL string
	// ApprovalsURL links the approval queue, where the apply waits after merge, empty when the
	// server has no public address.
	ApprovalsURL string
	// Receipt is the chain receipt of the webhook that requested the plan.
	Receipt string
	// CommitSHA is the commit planned.
	CommitSHA string
	// Tool is the run's execution tool.
	Tool string
	// Plan is the plan summary for a terraform or opentofu plan, nil for other tools.
	Plan *dispatch.PlanCounts
	// PlanRead reports that the plan summary was read, false when it could not be.
	PlanRead bool
	// Hosts are the per host results of an Ansible check, empty for other tools.
	Hosts []run.HostSummary
	// Preview is the decision the apply would get, nil until the plan has succeeded.
	Preview *dispatch.ApplyPreview
	// Excerpt is the plan output to show, already masked, empty when withheld.
	Excerpt string
	// ExcerptNote explains a shortened or withheld excerpt.
	ExcerptNote string
	// At orders reports: a report never replaces a comment written for a newer plan.
	At time.Time
	// Fork marks the refusal of a pull request from a fork, which sets a commit status and writes
	// no comment.
	Fork bool
	// NoHosts marks a plan skipped because the template's inventory matched no hosts, which sets a
	// commit status and writes no comment.
	NoHosts bool
	// Note is a sentence about the report itself, such as an outcome missing from the audit chain,
	// written above the closing line. Empty for none.
	Note string
}

// markerPattern reads the marker line a review writes as the first line of its comment. The commit
// is optional, since a comment written before the marker carried it has none.
var markerPattern = regexp.MustCompile(
	`<!-- switchtender-review template=(\S+) run=(\S*) at=(\d+)(?: commit=([0-9a-f]*))? -->`)

// MarkerPrefix returns the text identifying the review comment of one template, the part every
// version of that comment shares.
func MarkerPrefix(templateID string) string {
	return "<!-- switchtender-review template=" + templateID + " "
}

// marker returns the full marker line for rep.
func marker(rep Report) string {
	return fmt.Sprintf("<!-- switchtender-review template=%s run=%s at=%d commit=%s -->",
		rep.TemplateID, rep.RunID, rep.At.UnixNano(), strings.ToLower(rep.CommitSHA))
}

// markerInfo is what a comment's marker line says about the report that wrote it.
type markerInfo struct {
	// Run is the plan run, empty for a refusal.
	Run string
	// At is the report's plan time in nanoseconds.
	At int64
	// Commit is the commit the report described, empty for a comment written before markers
	// carried it.
	Commit string
}

// parseMarker reads body's marker line, reporting false when it has none.
func parseMarker(body string) (markerInfo, bool) {
	m := markerPattern.FindStringSubmatch(firstLine(body))
	if m == nil {
		return markerInfo{}, false
	}
	n, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return markerInfo{}, false
	}
	return markerInfo{Run: m[2], At: n, Commit: m[4]}, true
}

// Newer reports whether body, an existing review comment, describes a plan requested after at. A
// body with no readable marker is not newer, so it is replaced.
func Newer(body string, at time.Time) bool {
	m, ok := parseMarker(body)
	return ok && m.At > at.UnixNano()
}

// firstLine returns text up to its first newline.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// code renders s as inline code a pull request author cannot break out of: backticks and line
// breaks are replaced, so a branch name or rule label cannot open markup, mention anybody, or forge
// a second marker.
func code(s string) string {
	s = strings.NewReplacer("`", "'", "\r", " ", "\n", " ", "<", "‹", ">", "›").Replace(s)
	return "`" + s + "`"
}

// fence returns a code fence longer than any run of backticks in text, so output that prints a
// fence of its own cannot end the block early.
func fence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	return strings.Repeat("`", max(3, longest+1))
}

// shortSHA abbreviates a commit for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// headline returns the comment's opening sentence for the report's phase.
func headline(rep Report) string {
	switch rep.Phase {
	case PhaseRunning:
		return "**Plan running.** The result replaces this comment when it finishes."
	case PhaseHeld:
		held := "**Plan waiting for approval.** A rule holds this plan itself before it may run: " +
			code(rep.HeldBy) + "."
		if rep.RunURL != "" {
			return held + " [Open the plan in SwitchTender](" + rep.RunURL + ") to approve or " +
				"reject it."
		}
		return held + " An approver releases it in SwitchTender, where it is run " +
			code(rep.RunID) + "."
	case PhaseSucceeded:
		return "**Plan succeeded.** Nothing was applied."
	case PhaseCanceled:
		return "**Plan canceled.** No result to report."
	case PhaseRefused:
		return "**Not planned.** " + rep.Reason
	default:
		if rep.Reason != "" {
			return "**Plan failed.** " + rep.Reason
		}
		return "**Plan failed.** Nothing was applied."
	}
}

// Render returns the comment body for rep: a marker naming the template and the plan, the outcome,
// the plan summary and the destroy count the rules weigh, the decision the apply would get, the
// links, the receipt, and a bounded excerpt of the masked plan output.
func Render(rep Report) string {
	var b strings.Builder
	b.WriteString(marker(rep) + "\n")
	b.WriteString("### SwitchTender plan: " + code(rep.TemplateName) + "\n\n")
	b.WriteString(headline(rep) + "\n\n")
	var facts []string
	if rep.RunID != "" {
		facts = append(facts, "Plan "+code(PlanID(rep.RunID)))
	}
	if rep.CommitSHA != "" {
		facts = append(facts, "Commit "+code(shortSHA(rep.CommitSHA)))
	}
	if rep.RunID != "" {
		if rep.RunURL != "" {
			facts = append(facts, "Run ["+rep.RunID+"]("+rep.RunURL+")")
		} else {
			facts = append(facts, "Run "+code(rep.RunID))
		}
	}
	if rep.Receipt != "" {
		facts = append(facts, "Receipt "+code(rep.Receipt))
	}
	if len(facts) > 0 {
		b.WriteString(strings.Join(facts, " | ") + "\n\n")
	}
	if rep.Phase == PhaseSucceeded {
		writeSummary(&b, rep)
		writePreview(&b, rep)
		if rep.RunID != "" {
			b.WriteString("Where comment approvals are on, a linked approver applies exactly this " +
				"plan with " + code("/switchtender apply "+PlanID(rep.RunID)) + ".\n\n")
		}
	}
	if rep.Excerpt != "" {
		f := fence(rep.Excerpt)
		b.WriteString("<details><summary>Plan output</summary>\n\n" + f + "text\n" +
			strings.TrimRight(rep.Excerpt, "\n") + "\n" + f + "\n\n</details>\n\n")
	}
	if rep.ExcerptNote != "" {
		b.WriteString(rep.ExcerptNote + "\n\n")
	}
	if rep.Note != "" {
		b.WriteString(rep.Note + "\n\n")
	}
	b.WriteString("_Plan only: this run used the tool's no-change mode. An apply happens in " +
		"SwitchTender, through the normal gate and approval queue._\n")
	out := b.String()
	if len(out) > maxCommentBytes {
		out = strings.ToValidUTF8(out[:maxCommentBytes], "") + "\n\n_Comment truncated._\n"
	}
	return out
}

// writeSummary writes the plan's change counts, or the per host check results for Ansible.
func writeSummary(b *strings.Builder, rep Report) {
	switch {
	case rep.Plan != nil && rep.PlanRead:
		p := rep.Plan
		b.WriteString("| Add | Change | Destroy | Import |\n|---:|---:|---:|---:|\n")
		fmt.Fprintf(b, "| %d | %d | %d | %d |\n\n", p.Add, p.Change, p.Destroy, p.Import)
		fmt.Fprintf(b, "Destroy count the rules weigh: **%d**\n\n", p.Destroy)
	case rep.Plan != nil:
		b.WriteString("The plan's change summary could not be read, so its destroy count is " +
			"unknown. The rules treat an unread plan as one that must be held.\n\n")
	case len(rep.Hosts) > 0:
		changed, failed := 0, 0
		b.WriteString("| Host | Would change | OK | Failed | Unreachable |\n|---|---:|---:|---:|---:|\n")
		for i, h := range rep.Hosts {
			if h.Changed > 0 {
				changed++
			}
			if h.Failures > 0 || h.Unreachable > 0 {
				failed++
			}
			if i < 50 {
				fmt.Fprintf(b, "| %s | %d | %d | %d | %d |\n", code(h.Host), h.Changed, h.OK, h.Failures,
					h.Unreachable)
			}
		}
		if len(rep.Hosts) > 50 {
			fmt.Fprintf(b, "| %d more hosts | | | | |\n", len(rep.Hosts)-50)
		}
		fmt.Fprintf(b, "\n%d of %d hosts would change.\n\n", changed, len(rep.Hosts))
	}
}

// writePreview writes the decision the apply would get after merge.
func writePreview(b *strings.Builder, rep Report) {
	p := rep.Preview
	if p == nil {
		return
	}
	where := "when it is submitted"
	if p.Stage == dispatch.StagePlan {
		where = "when its plan is weighed"
	}
	switch p.Outcome {
	case dispatch.ApplyDenied:
		b.WriteString("**After merge, the apply would be refused** " + where + " by rule " +
			code(p.Rule) + ". Change the rule or the code before merging.\n\n")
	case dispatch.ApplyHeld:
		b.WriteString("**After merge, the apply would be held for approval** " + where +
			" by rule " + code(p.Rule) + ". An operator or admin with use access to this template " +
			"releases it in SwitchTender")
		if p.RequireDistinctApprover {
			b.WriteString(", and the rule requires someone other than the person who requests it")
		}
		b.WriteString(".")
		if rep.ApprovalsURL != "" {
			b.WriteString(" [Approval queue](" + rep.ApprovalsURL + ")")
		}
		b.WriteString("\n\n")
	default:
		b.WriteString("**After merge, no rule would hold the apply.**")
		if p.PlanGated {
			b.WriteString(" It is planned again first and weighed on that plan's destroy count.")
		}
		b.WriteString("\n\n")
	}
}

// StatusFor returns the commit status for rep under context name.
func StatusFor(rep Report, context string) Status {
	s := Status{Context: context, TargetURL: rep.RunURL}
	switch rep.Phase {
	case PhaseRunning:
		s.State, s.Description = StatePending, "Plan running in SwitchTender"
	case PhaseHeld:
		s.State, s.Description = StatePending, "Plan waiting for approval: "+rep.HeldBy
	case PhaseSucceeded:
		s.State, s.Description = StateSuccess, "Plan succeeded"
		if p := rep.Preview; p != nil {
			switch p.Outcome {
			case dispatch.ApplyDenied:
				s.State, s.Description = StateFailure, "Plan succeeded, apply would be refused by "+p.Rule
			case dispatch.ApplyHeld:
				s.Description = "Plan succeeded, apply needs approval: " + p.Rule
			default:
				s.Description = "Plan succeeded, no rule would hold the apply"
			}
		}
		if rep.Plan != nil && rep.PlanRead {
			s.Description += fmt.Sprintf(" (destroys %d)", rep.Plan.Destroy)
		}
	case PhaseCanceled:
		s.State, s.Description = StateError, "Plan canceled"
	case PhaseRefused:
		s.State, s.Description = StateError, "Not planned: "+rep.Reason
		switch {
		case rep.Fork:
			s.Description, s.TargetURL = ForkStatus, ForkDocsURL
		case rep.NoHosts:
			s.Description, s.TargetURL = NoHostsStatus, NoHostsDocsURL
		}
	default:
		s.State, s.Description = StateFailure, "Plan failed"
	}
	if len(s.Description) > maxDescription {
		s.Description = s.Description[:maxDescription-3] + "..."
	}
	return s
}

// Excerpt returns the part of a plan's masked output worth showing and a note when it was
// shortened. A terraform or opentofu plan is shown from where it starts describing actions, since
// what precedes that is provider initialization. Other output is shown from its end, where Ansible
// prints its recap. Either way it is bounded, and the note says so and points at the full log.
func Excerpt(out, tool, logURL string) (string, string) {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	tool = run.NormalizeTool(tool)
	terraform := tool == run.ToolTerraform || tool == run.ToolOpenTofu
	if terraform {
		for _, start := range []string{"Terraform will perform", "OpenTofu will perform",
			"No changes.", "Changes to Outputs:"} {
			if i := strings.Index(out, start); i >= 0 {
				out = out[i:]
				break
			}
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	total := len(lines)
	shortened := false
	if total > maxExcerptLines {
		shortened = true
		if terraform {
			lines = lines[:maxExcerptLines]
		} else {
			lines = lines[total-maxExcerptLines:]
		}
	}
	text := strings.Join(lines, "\n")
	if len(text) > maxExcerptBytes {
		shortened = true
		if terraform {
			text = text[:maxExcerptBytes]
		} else {
			text = text[len(text)-maxExcerptBytes:]
		}
		text = strings.ToValidUTF8(text, "")
	}
	if !shortened {
		return text, ""
	}
	note := fmt.Sprintf("_Output shortened to %d of %d lines._", strings.Count(text, "\n")+1, total)
	if logURL != "" {
		note += " [Full log](" + logURL + ")"
	}
	return text, note
}
