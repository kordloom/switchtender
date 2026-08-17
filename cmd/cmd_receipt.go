package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/receipt"
)

var (
	// receiptRunDB is the database holding the run and the chain the receipt is drawn from.
	receiptRunDB = defaultDBPath
	// receiptRunOut is the file the signed receipt is written to, empty for stdout.
	receiptRunOut string
	// receiptSparse emits the receipt on the tree profile, disclosing only this run's entries.
	receiptSparse bool
	// receiptFrom proves the log only appended since an earlier size, when above zero.
	receiptFrom int64
)

// receiptCmd produces a signed, offline-verifiable receipt for one run.
var receiptCmd = &cobra.Command{
	Use:   "receipt <run-id>",
	Short: "Write a signed, offline-verifiable receipt for one run.",
	Long: `Write a signed receipt for one run that a third party verifies offline with switchtender verify.

A receipt is the chain segment from the request that created the run through the entry that recorded
what the run did, signed with this install's key and carrying any anchors that fix its position.
Verifying it needs no database and no network and does not trust this server: the signature proves
the bytes are unaltered, and every chain link recomputes from the claims alone.

The run must have finished, so its outcome is on the chain. Publish the key fingerprint the command
prints so a verifier can pin it.`,
	Args: cobra.ExactArgs(1),
	RunE: runReceipt,
}

// init registers the receipt command and its flags.
func init() {
	receiptCmd.Flags().StringVar(&receiptRunDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, holding the run and its chain.")
	receiptCmd.Flags().StringVar(&receiptRunOut, "out", "",
		"File to write the receipt to. Defaults to stdout.")
	receiptCmd.Flags().BoolVar(&receiptSparse, "sparse", false,
		"Disclose only this run's own chain entries, proving each belongs to the log without "+
			"carrying the entries around it.")
	receiptCmd.Flags().Int64Var(&receiptFrom, "append-only-from", 0,
		"With --sparse, prove the log only appended since this size. Use a size a reader already "+
			"saw, such as an anchored head.")
	rootCmd.AddCommand(receiptCmd)
}

// runReceipt assembles and signs the receipt for one run. The assembly lives in internal/receipt so
// the command and the HTTP endpoint produce the same bytes.
func runReceipt(cmd *cobra.Command, args []string) error {
	runID := args[0]
	store, err := openBundle(receiptRunDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	id, err := audit.LoadIdentity(identityDir(receiptRunDB))
	if err != nil {
		return err
	}
	res, err := receipt.Build(cmd.Context(), store.Runs(), store.Audits(), id, resolveVersion(),
		runID, receipt.Options{Sparse: receiptSparse, From: receiptFrom})
	if err != nil {
		return err
	}
	for _, note := range res.Notes {
		fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	if res.UnanchoredSparse {
		fmt.Fprintln(cmd.ErrOrStderr(), "No tree anchor covers this receipt, so nothing outside "+
			"this install fixes the root it proves membership in. Run switchtender audit anchor "+
			"--tree and issue the receipt again, or pass --append-only-from with a size a tree "+
			"anchor already fixed.")
	}
	if res.Anchors > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Attached %d anchor(s) covering the receipt.\n", res.Anchors)
	}
	if receiptSparse && receiptFrom > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Proving the log only appended since size %d.\n", receiptFrom)
	}

	if receiptRunOut == "" {
		_, err = os.Stdout.Write(res.Signed)
		return err
	}
	if err := os.WriteFile(receiptRunOut, res.Signed, 0o644); err != nil {
		return fmt.Errorf("write receipt: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Wrote a receipt for run %s to %s (%d chain entries).\n"+
			"Verify it with: switchtender verify %s\n"+
			"Publish this fingerprint so a verifier can pin it:\n  %s\n",
		runID, receiptRunOut, res.Claims, receiptRunOut, res.KeyID)
	return nil
}
