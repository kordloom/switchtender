package cmd

import (
	"fmt"
	"io"
	"os"

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
	if rep.OutcomePresent {
		fmt.Fprintf(out, "outcome      %s (matches what the chain committed)\n", mark(rep.OutcomeDigestOK))
		if rep.OutcomeDigestOK {
			printOutcome(out, rep.OutcomeBody)
		}
	}

	if !rep.OK() {
		fmt.Fprintln(out, "\nNOT VERIFIED")
		return fmt.Errorf("receipt did not verify")
	}
	fmt.Fprintln(out, "\nVERIFIED: nothing has been altered since this receipt was signed")
	return nil
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
	if rec.Image != "" {
		fmt.Fprintf(out, "  image          %s\n", rec.Image)
	}
	for _, h := range rec.Hosts {
		fmt.Fprintf(out, "  host %-16s %s  ok=%d changed=%d failed=%d unreachable=%d\n",
			h.Host, h.Worst, h.OK, h.Changed, h.Failures, h.Unreachable)
	}
	fmt.Fprintf(out, "  log sha256     %s\n", rec.LogSHA256)
}
