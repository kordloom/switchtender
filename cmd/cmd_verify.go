package cmd

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// verifyPubkey is the key fingerprint a relying party pins, so a receipt signed by any other key is
// refused. Empty trusts whatever key the receipt names, which checks integrity but not provenance.
var verifyPubkey string

// verifyCmd checks a run receipt offline.
var verifyCmd = &cobra.Command{
	Use:   "verify <receipt-file>",
	Short: "Verify a run receipt offline, with no database and no network.",
	Long: `Verify a run receipt written by switchtender receipt.

This trusts nothing this server says. It recomputes every chain link from the receipt's own claims,
checks the producer's signature covers the exact bytes, and confirms any anchors name an entry the
receipt holds. It reads only the file, reaches no database and no network, and does not run this
server, so a relying party can check a receipt on a machine that has never seen this install.

Pass --pubkey with the fingerprint the producer published to tie the result to a key you obtained out
of band. Without it the receipt is checked against the key it names, which proves it was not altered
but not who signed it, and the verdict reads INTACT, BUT UNIDENTIFIED instead of VERIFIED. An empty
--pubkey is refused, since that is what a failed key fetch leaves behind.`,
	Args: cobra.ExactArgs(1),
	RunE: runVerify,
}

// init registers the verify command.
func init() {
	verifyCmd.Flags().StringVar(&verifyPubkey, "pubkey", "",
		"Key fingerprint (sha256:...) to pin, refusing a receipt signed by any other key.")
	rootCmd.AddCommand(verifyCmd)
}

// runVerify checks one receipt file and reports the verdict.
func runVerify(cmd *cobra.Command, args []string) error {
	if err := refuseEmptyPin(cmd, "pubkey", verifyPubkey); err != nil {
		return err
	}
	signed, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read receipt: %w", err)
	}
	rep, err := audit.VerifyBundle(signed, verifyPubkey)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	mark := func(ok bool) string {
		if ok {
			return "OK"
		}
		return "FAILED"
	}
	fmt.Fprintf(out, "subject      %s %s\n", rep.Subject.Type, rep.Subject.ID)
	fmt.Fprintf(out, "signed by    %s\n", rep.KeyID)
	fmt.Fprintf(out, "signature    %s\n", mark(rep.SignatureOK))
	// Whether a key was pinned decides what the verdict can claim, so it gets its own line rather
	// than being left for a reader to infer from a flag they may not have typed. The wording is the
	// browser verifier's, so the two never drift in a reader's memory.
	pinned := strings.TrimSpace(verifyPubkey) != ""
	if pinned {
		fmt.Fprintln(out, "pin          OK (matches the fingerprint you pinned, so this is that install's key)")
	} else {
		fmt.Fprintln(out, "pin          NONE (this says the receipt was signed, not who signed it)")
	}
	if rep.ChainOK {
		fmt.Fprintf(out, "chain        OK (%d entries recompute, head seq %d)\n", rep.ClaimCount, rep.Head.Seq)
	} else {
		fmt.Fprintf(out, "chain        FAILED (does not recompute at seq %d)\n", rep.BrokeAtSeq)
	}
	if rep.AnchorCount > 0 {
		// The count of anchors was the whole line, which said nothing about the one part of an anchor
		// that does not come from the producer: the authority's own token. A reader has to be able to
		// tell an anchor this tool actually read from one it merely counted.
		detail := fmt.Sprintf("%d", rep.AnchorCount)
		switch {
		case rep.TimestampsVerified > 0:
			detail += fmt.Sprintf(", %d with a timestamp token that fixes this chain",
				rep.TimestampsVerified)
		case len(rep.TimestampProblems) == 0:
			detail += ", none carrying a timestamp token, so their positions rest on where they " +
				"were published"
		}
		fmt.Fprintf(out, "anchors      %s (%s)\n", mark(rep.AnchorsOK), detail)
		// What was checked is that the token commits to this chain's link. Whether the authority that
		// issued it is worth believing is a trust decision this tool cannot make for a reader, and a
		// count with nothing said about it invites reading it as one we did make. It goes on its own
		// line beside the problems, rather than as a second parenthetical inside the first.
		if rep.TimestampsVerified > 0 {
			fmt.Fprintln(out, "  the issuing authority's signature is yours to check")
		}
		for _, p := range rep.TimestampProblems {
			fmt.Fprintf(out, "  %s\n", p)
		}
	} else {
		fmt.Fprintln(out, "anchors      none (nothing outside this install fixes its position)")
	}
	// A caption states what the result means, so it has to follow the result. The parenthetical
	// used to assert the passing case beside a FAILED, which read as a verifier contradicting
	// itself on the one line a reader looks at hardest.
	if rep.OutcomePresent {
		fmt.Fprintf(out, "outcome      %s (%s what the chain committed)\n",
			mark(rep.OutcomeDigestOK), matchWord(rep.OutcomeDigestOK))
		if rep.OutcomeDigestOK {
			printOutcome(out, rep.OutcomeBody)
		}
	}
	if rep.DecisionsPresent > 0 {
		fmt.Fprintf(out, "decisions    %s (%d, each %s what the chain committed)\n",
			mark(rep.DecisionsOK), rep.DecisionsPresent, matchWord(rep.DecisionsOK))
		for _, d := range rep.Decisions {
			fmt.Fprintf(out, "  %s %s, binding spec %s\n", d.Verdict, decidedBy(d), d.SpecDigest)
		}
	}
	if rep.SpecPresent || rep.DecisionsPresent > 0 {
		fmt.Fprintf(out, "spec         %s (%s)\n", mark(rep.SpecConsistent), specVerdict(rep))
	}

	if !rep.OK() {
		fmt.Fprintln(out, "\nNOT VERIFIED: "+failedChecks(rep))
		return fmt.Errorf("receipt did not verify: %s", failedChecks(rep))
	}
	// Without a pin a forged receipt earned the same VERIFIED as a genuine one, because any key signs
	// its own bundle. The unpinned result is its own verdict, the one the browser verifier gives.
	if !pinned {
		fmt.Fprintln(out, "\nINTACT, BUT UNIDENTIFIED: nothing has been altered since this receipt was "+
			"signed. Who signed it is unchecked, because no key was pinned. Pass --pubkey with the "+
			"fingerprint the producing install publishes at /.well-known/loomseal.json.")
		return nil
	}
	fmt.Fprintln(out, "\nVERIFIED: nothing has been altered since this receipt was signed")
	return nil
}

// refuseEmptyPin refuses a pin flag that was given with nothing in it. A pin is usually filled in by
// a command that fetches the published key, and when that fetch fails the flag arrives empty: the
// verifier then checked against whatever key the file named and reported success, so a pin that was
// asked for silently became none at all.
func refuseEmptyPin(cmd *cobra.Command, flag, value string) error {
	if cmd.Flags().Changed(flag) && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: --%s was given an empty value, most often because the command that "+
			"fetched the key failed. Pass the published fingerprint, or leave --%s off to check "+
			"integrity alone", errEmptyPin, flag, flag)
	}
	return nil
}

// shortDigest renders a digest at a length a person can compare across two receipts by eye, which is
// what a reader actually does with it: the full value is in the record either way.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// matchWord is the caption verb for a digest comparison, in the tense the result calls for.
func matchWord(ok bool) string {
	if ok {
		return "matches"
	}
	return "does not match"
}

// failedChecks names the checks that did not pass, so a refusal says what is wrong rather than only
// that something is. A reader holding a refused receipt needs to know whether the signature failed,
// the chain broke, or the approved change is not the change that ran; those are different problems
// with different owners.
func failedChecks(rep *audit.BundleReport) string {
	var failed []string
	if !rep.SignatureOK {
		failed = append(failed, "the signature does not cover these bytes")
	}
	if !rep.ChainOK {
		failed = append(failed, fmt.Sprintf("the chain does not recompute at seq %d", rep.BrokeAtSeq))
	}
	// Only when the receipt carries an anchor. Anchors are not trusted over a chain that does not
	// recompute, so a broken chain marks them failed too, and a receipt holding no anchor at all was
	// reported as having one that does not prove its position. That sends a reader looking for a
	// problem with evidence the receipt never claimed to have.
	if !rep.AnchorsOK && rep.AnchorCount > 0 {
		if len(rep.TimestampProblems) > 0 {
			failed = append(failed, "a timestamp token does not fix the link its anchor names")
		} else {
			failed = append(failed, "an anchor names a position this receipt does not prove")
		}
	}
	if rep.OutcomePresent && !rep.OutcomeDigestOK {
		failed = append(failed, "the disclosed outcome is not what the chain committed")
	}
	if !rep.DecisionsOK {
		failed = append(failed, "a disclosed decision is not what the chain committed")
	}
	if !rep.SpecConsistent {
		if approvedAndExecuted(rep) {
			failed = append(failed, "the approved and the executed change are not the same")
		} else {
			failed = append(failed, "the spec digests this receipt discloses do not agree")
		}
	}
	if len(failed) == 0 {
		return "a check did not pass"
	}
	return strings.Join(failed, "; ")
}

// decidedBy says who made a decision, as far as the chain recorded it. An install with no tokens
// records a decision with no actor, and printing the empty fields made the line read "by  ()".
func decidedBy(d audit.DisclosedDecision) string {
	switch {
	case d.Actor == "":
		return "with no actor recorded"
	case d.ActorType == "":
		return "by " + d.Actor
	default:
		return "by " + d.Actor + " (" + d.ActorType + ")"
	}
}

// specVerdict says which spec digests the receipt let the spec check compare and whether they
// agreed, naming only the ones present. The line used to name the approved, executed, and disclosed
// digests on every receipt, which told a reader that a change which was rejected had been approved
// and executed.
func specVerdict(rep *audit.BundleReport) string {
	var names []string
	for _, verdict := range []string{"approved", "rejected"} {
		if hasVerdict(rep, verdict) {
			names = append(names, verdict)
		}
	}
	if name := outcomeName(rep); name != "" && !slices.Contains(names, name) {
		names = append(names, name)
	}
	if rep.SpecPresent {
		names = append(names, "disclosed")
	}
	var subject string
	switch len(names) {
	case 0:
		return "no spec digest to compare"
	case 1:
		subject = names[0] + " digests"
	case 2:
		subject = names[0] + " and " + names[1] + " digests"
	default:
		subject = strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1] + " digests"
	}
	switch {
	case rep.SpecConsistent:
		return subject + " agree"
	case approvedAndExecuted(rep):
		return subject + " do not agree, so the change that was approved is not the change that ran"
	default:
		return subject + " do not agree"
	}
}

// approvedAndExecuted reports whether the receipt discloses an approval and the outcome of a run that
// executed, the only case where a spec disagreement means the change that ran is not the change that
// was approved.
func approvedAndExecuted(rep *audit.BundleReport) bool {
	return outcomeName(rep) == "executed" && hasVerdict(rep, "approved")
}

// outcomeName names the verified outcome record the way the spec line reads it: executed for a run
// that ran, and its terminal status for one that ended otherwise, so a rejected run is never called
// executed. It is empty when there is no verified outcome, which the spec check does not compare.
func outcomeName(rep *audit.BundleReport) string {
	if !rep.OutcomePresent || !rep.OutcomeDigestOK {
		return ""
	}
	rec, err := outcome.Parse(rep.OutcomeBody)
	if err != nil || rec.Status == "" {
		return ""
	}
	switch run.Status(rec.Status) {
	case run.StatusSucceeded, run.StatusFailed, run.StatusInterrupted:
		return "executed"
	default:
		return rec.Status
	}
}

// hasVerdict reports whether the receipt discloses a decision with the given verdict.
func hasVerdict(rep *audit.BundleReport, verdict string) bool {
	return slices.ContainsFunc(rep.Decisions, func(d audit.DisclosedDecision) bool {
		return d.Verdict == verdict
	})
}

// printOutcome renders what the run did from the disclosed, digest-verified outcome, so a reader sees
// the result and not only that the record is intact.
func printOutcome(out io.Writer, body []byte) {
	rec, err := outcome.Parse(body)
	if err != nil {
		return
	}
	exit := "none"
	if rec.ExitCode != nil {
		exit = fmt.Sprintf("%d", *rec.ExitCode)
	}
	fmt.Fprintf(out, "  what happened  run %s %s (exit %s)\n", rec.RunID, rec.Status, exit)
	// A no-change preview and the change itself are the two things a reader must never confuse, and
	// the record distinguishes them. Leaving the mode out made a receipt for a dry run read exactly
	// like a receipt for the real thing. The commit is the content that actually ran.
	if rec.DryRun {
		fmt.Fprintln(out, "  mode           check mode, so nothing was changed")
	}
	if rec.CommitSHA != "" {
		fmt.Fprintf(out, "  commit         %s\n", rec.CommitSHA)
	}
	if rec.SpecDigest != "" {
		fmt.Fprintf(out, "  spec digest    %s\n", rec.SpecDigest)
	}
	// The rules that were in force, which is what makes "nothing stopped this" a statement rather than
	// an absence. A receipt from an install with no rules says so in as many words.
	if set := rec.PolicySet; set != nil {
		if set.Count == 0 {
			fmt.Fprintf(out, "  rules in force none recorded at submit (%s)\n", shortDigest(set.Digest))
		} else {
			fmt.Fprintf(out, "  rules in force %d (%s)\n", set.Count, shortDigest(set.Digest))
			for _, rule := range set.Rules {
				fmt.Fprintf(out, "    - %s\n", rule)
			}
		}
	}
	if rec.Image != "" {
		fmt.Fprintf(out, "  image          %s\n", rec.Image)
	}
	for _, h := range rec.Hosts {
		fmt.Fprintf(out, "  host %-16s %s  ok=%d changed=%d failed=%d unreachable=%d\n",
			h.Host, h.Worst, h.OK, h.Changed, h.Failures, h.Unreachable)
	}
	// A split or pipeline coordinator has no hosts of its own; the execution lives in its children.
	// Not rendering them made the flagship offline artifact read "nothing happened" over a fan-out
	// that ran on real machines, then say VERIFIED, which is exactly the shape a skeptic distrusts.
	for _, c := range rec.Children {
		name := c.Name
		if name == "" && c.Index != nil {
			name = fmt.Sprintf("shard %d", *c.Index)
		}
		exit := "none"
		if c.ExitCode != nil {
			exit = fmt.Sprintf("%d", *c.ExitCode)
		}
		attempt := ""
		if c.Attempt > 1 {
			attempt = fmt.Sprintf(" attempt %d", c.Attempt)
		}
		fmt.Fprintf(out, "  child %-15s %s (exit %s, run %s%s)\n", name, c.Status, exit, c.RunID, attempt)
	}
	fmt.Fprintf(out, "  log sha256     %s\n", rec.LogSHA256)
}
