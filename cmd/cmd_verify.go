package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
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
but not who signed it.`,
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
	if rep.ChainOK {
		fmt.Fprintf(out, "chain        OK (%d entries recompute, head seq %d)\n", rep.ClaimCount, rep.Head.Seq)
	} else {
		fmt.Fprintf(out, "chain        FAILED (does not recompute at seq %d)\n", rep.BrokeAtSeq)
	}
	if rep.AnchorCount > 0 {
		fmt.Fprintf(out, "anchors      %s (%d)\n", mark(rep.AnchorsOK), rep.AnchorCount)
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
			fmt.Fprintf(out, "  %s by %s (%s), binding spec %s\n",
				d.Verdict, d.Actor, d.ActorType, d.SpecDigest)
		}
	}
	if rep.SpecPresent || rep.DecisionsPresent > 0 {
		agree := "agree"
		if !rep.SpecConsistent {
			agree = "do not agree, so the change that was approved is not the change that ran"
		}
		fmt.Fprintf(out, "spec         %s (approved, executed, and disclosed digests %s)\n",
			mark(rep.SpecConsistent), agree)
	}

	if !rep.OK() {
		fmt.Fprintln(out, "\nNOT VERIFIED: "+failedChecks(rep))
		return fmt.Errorf("receipt did not verify: %s", failedChecks(rep))
	}
	fmt.Fprintln(out, "\nVERIFIED: nothing has been altered since this receipt was signed")
	return nil
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
	if !rep.AnchorsOK {
		failed = append(failed, "an anchor names a position this receipt does not prove")
	}
	if rep.OutcomePresent && !rep.OutcomeDigestOK {
		failed = append(failed, "the disclosed outcome is not what the chain committed")
	}
	if !rep.DecisionsOK {
		failed = append(failed, "a disclosed decision is not what the chain committed")
	}
	if !rep.SpecConsistent {
		failed = append(failed, "the approved and the executed change are not the same")
	}
	if len(failed) == 0 {
		return "a check did not pass"
	}
	return strings.Join(failed, "; ")
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
	if rec.Image != "" {
		fmt.Fprintf(out, "  image          %s\n", rec.Image)
	}
	for _, h := range rec.Hosts {
		fmt.Fprintf(out, "  host %-16s %s  ok=%d changed=%d failed=%d unreachable=%d\n",
			h.Host, h.Worst, h.OK, h.Changed, h.Failures, h.Unreachable)
	}
	fmt.Fprintf(out, "  log sha256     %s\n", rec.LogSHA256)
}
