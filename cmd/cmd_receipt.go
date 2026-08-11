package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
)

var (
	// receiptRunDB is the database holding the run and the chain the receipt is drawn from.
	receiptRunDB = defaultDBPath
	// receiptRunOut is the file the signed receipt is written to, empty for stdout.
	receiptRunOut string
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
	rootCmd.AddCommand(receiptCmd)
}

// runReceipt assembles and signs the receipt for one run.
func runReceipt(cmd *cobra.Command, args []string) error {
	runID := args[0]
	store, err := openBundle(receiptRunDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	r, err := store.Runs().Get(cmd.Context(), runID)
	if err != nil {
		return fmt.Errorf("read run %s: %w", runID, err)
	}
	if r.AuditReceipt == "" {
		return fmt.Errorf("run %s has no creation receipt, so its start cannot be placed on the "+
			"chain", runID)
	}
	creationSeq, _, err := parseReceipt(r.AuditReceipt)
	if err != nil {
		return fmt.Errorf("run %s carries an unreadable creation receipt: %w", runID, err)
	}

	entries, err := store.Audits().Chain(cmd.Context())
	if err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	outcomePrefix := "/runs/" + runID + "/outcome/"
	var outcomeSeq int64
	var outcomeEntry *audit.Entry
	for _, e := range entries {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, outcomePrefix) {
			outcomeSeq = e.Seq
			outcomeEntry = e
		}
	}
	if outcomeSeq == 0 {
		return fmt.Errorf("run %s has no committed outcome yet, so there is nothing to receipt. A "+
			"run is receiptable once it has finished", runID)
	}
	if outcomeSeq < creationSeq {
		return fmt.Errorf("run %s: its outcome is recorded before its creation, which is a broken "+
			"chain", runID)
	}

	segment := make([]*audit.Entry, 0, outcomeSeq-creationSeq+1)
	for _, e := range entries {
		if e.Seq >= creationSeq && e.Seq <= outcomeSeq {
			segment = append(segment, e)
		}
	}

	id, err := audit.LoadIdentity(identityDir(receiptRunDB))
	if err != nil {
		return err
	}
	doc, err := audit.BuildBundle(segment, id, resolveVersion(), time.Now())
	if err != nil {
		return err
	}
	// The receipt is about the run, not the whole fleet, so it names the run as its subject.
	doc.Subject = audit.BundleSubject{Type: "run", ID: runID}

	// Disclose the run's outcome inside the outcome claim, so verify can show what the run did and
	// prove it matches the digest the chain committed. The body is rebuilt from the run's evidence and
	// the nonce is the one the digest was keyed under; both are added to the claim payload, which the
	// signature then covers. They are not part of the chain link, so they do not change any claim's
	// verification, exactly as a span claim's extra members do not.
	if body, berr := outcome.Body(cmd.Context(), store.Runs(), r); berr == nil {
		var bodyObj any
		if json.Unmarshal(body, &bodyObj) == nil {
			for i := range doc.Claims {
				if doc.Claims[i].Chain.Seq == outcomeSeq {
					doc.Claims[i].Payload["outcome_body"] = bodyObj
					doc.Claims[i].Payload["outcome_nonce"] = outcomeEntry.Nonce
				}
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "Could not rebuild the outcome to disclose it: %v. The receipt still "+
			"proves the chain, but verify will not show what the run did.\n", berr)
	}

	// Anchors that fix a position in the receipt's range are attached before signing, so a verifier
	// can see the segment has not been shortened. The whole chain is held against every anchor first,
	// the same discipline the full bundle uses, so a receipt is never published from a chain that
	// cannot satisfy an anchor recorded over it.
	if anchors, ok := store.Audits().(audit.AnchorStore); ok {
		recorded, aerr := anchors.Anchors(cmd.Context(), 0)
		if aerr != nil {
			return fmt.Errorf("read anchors: %w", aerr)
		}
		if reachedAll, results := audit.CheckAnchors(entries, recorded); !reachedAll {
			for _, res := range results {
				if !res.Reached {
					fmt.Fprintln(os.Stderr, "anchor "+res.Anchor.ID+": "+res.Problem)
				}
			}
			return fmt.Errorf("this chain does not satisfy every anchor recorded over it, so a " +
				"receipt drawn from it must not be published as one that does")
		}
		if n := doc.AttachAnchors(recorded); n > 0 {
			fmt.Fprintf(os.Stderr, "Attached %d anchor(s) covering the receipt.\n", n)
		}
	}

	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		return err
	}
	signed = append(signed, '\n')

	if receiptRunOut == "" {
		_, err = os.Stdout.Write(signed)
		return err
	}
	if err := os.WriteFile(receiptRunOut, signed, 0o644); err != nil {
		return fmt.Errorf("write receipt: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Wrote a receipt for run %s to %s (%d chain entries).\n"+
			"Verify it with: switchtender verify %s\n"+
			"Publish this fingerprint so a verifier can pin it:\n  %s\n",
		runID, receiptRunOut, len(segment), receiptRunOut, id.KeyID())
	return nil
}
