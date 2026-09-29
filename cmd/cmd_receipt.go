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
	"github.com/kordloom/switchtender/internal/run"
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

	id, err := audit.LoadIdentity(identityDir(receiptRunDB))
	if err != nil {
		return err
	}
	// The receipt is about the run, not the whole fleet, so it names the run as its subject.
	subject := audit.BundleSubject{Type: "run", ID: runID}

	var doc *audit.Bundle
	if receiptSparse {
		doc, err = sparseReceipt(cmd, entries, runID, creationSeq, outcomeSeq, id, subject)
	} else {
		doc, err = rangeReceipt(entries, creationSeq, outcomeSeq, id, subject)
	}
	if err != nil {
		return err
	}
	segment := doc.Claims

	// Disclose the run's outcome inside the outcome claim, so verify can show what the run did and
	// prove it matches the digest the chain committed. A sparse receipt cannot carry this: its leaf
	// hashes are computed over the chain's own entries, so a payload member added here would not
	// recompute, and making the leaf depend on which run is being receipted would give the same log
	// a different root per receipt. The body is rebuilt from the run's evidence and
	// the nonce is the one the digest was keyed under; both are added to the claim payload, which the
	// signature then covers. They are not part of the chain link, so they do not change any claim's
	// verification, exactly as a span claim's extra members do not.
	if body, berr := outcome.Body(cmd.Context(), store.Runs(), r); berr == nil && !receiptSparse {
		var bodyObj any
		if json.Unmarshal(body, &bodyObj) == nil {
			for i := range doc.Claims {
				if doc.Claims[i].Chain.Seq == outcomeSeq {
					doc.Claims[i].Payload["outcome_body"] = bodyObj
					doc.Claims[i].Payload["outcome_nonce"] = outcomeEntry.Nonce
					discloseSpec(&doc.Claims[i], r)
				}
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "Could not rebuild the outcome to disclose it: %v. The receipt still "+
			"proves the chain, but verify will not show what the run did.\n", berr)
	}
	if !receiptSparse {
		discloseDecisions(doc, entries, r)
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
		if reachedAll, results := audit.CheckAnchors(entries, recorded, id.InstallID); !reachedAll {
			for _, res := range results {
				if !res.Reached {
					fmt.Fprintln(os.Stderr, "anchor "+res.Anchor.ID+": "+res.Problem)
				}
			}
			return fmt.Errorf("this chain does not satisfy every anchor recorded over it, so a " +
				"receipt drawn from it must not be published as one that does")
		}
		n := doc.AttachAnchors(recorded)
		if n > 0 {
			fmt.Fprintf(os.Stderr, "Attached %d anchor(s) covering the receipt.\n", n)
		}
		// A sparse receipt proves membership in a tree coordinate, which only a tree anchor can
		// fix, so shipping one silently unanchored would hand over a receipt whose root rests on
		// this install's word alone.
		if receiptSparse && n == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "No tree anchor covers this receipt, so nothing "+
				"outside this install fixes the root it proves membership in. Run switchtender "+
				"audit anchor --tree and issue the receipt again, or pass --append-only-from "+
				"with a size a tree anchor already fixed.")
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

// discloseSpec attaches the run's redacted spec to the outcome claim, so a verifier can read what
// the digests commit to and recompute them. The spec is redacted before it leaves the outcome
// package, so this never hands out bytes the redaction did not pass over.
func discloseSpec(claim *audit.BundleClaim, r *run.Run) {
	spec, err := outcome.Spec(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not rebuild the spec to disclose it: %v.\n", err)
		return
	}
	var specObj any
	if json.Unmarshal(spec, &specObj) == nil {
		claim.Payload["spec_body"] = specObj
	}
}

// discloseDecisions attaches each approval decision's body and nonce to its claim, the same way the
// outcome is disclosed, so verify can show who approved exactly what and prove the chain committed
// it. The bodies are rebuilt from the run, so a row that changed since the decision produces a body
// the committed digest refuses, which is the tamper this disclosure exists to surface.
func discloseDecisions(doc *audit.Bundle, entries []*audit.Entry, r *run.Run) {
	prefix := "/runs/" + r.ID + "/decision/"
	for _, e := range entries {
		if e.Method != audit.MethodDecision || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		body, _, err := outcome.DecisionBody(r, strings.TrimPrefix(e.Path, prefix))
		if err != nil {
			continue
		}
		var bodyObj any
		if json.Unmarshal(body, &bodyObj) != nil {
			continue
		}
		for i := range doc.Claims {
			if doc.Claims[i].Chain.Seq == e.Seq {
				doc.Claims[i].Payload["decision_body"] = bodyObj
				doc.Claims[i].Payload["decision_nonce"] = e.Nonce
			}
		}
	}
}

// rangeReceipt builds the contiguous receipt: the chain segment from the request that created the run
// through the entry that recorded what it did. It can disclose the run's outcome, because its claims
// are not leaves of a tree, but on a busy install the segment also carries the entries that happened
// to be recorded between them.
func rangeReceipt(entries []*audit.Entry, creationSeq, outcomeSeq int64, id audit.Identity,
	subject audit.BundleSubject) (*audit.Bundle, error) {
	segment := make([]*audit.Entry, 0, outcomeSeq-creationSeq+1)
	for _, e := range entries {
		if e.Seq >= creationSeq && e.Seq <= outcomeSeq {
			segment = append(segment, e)
		}
	}
	doc, err := audit.BuildBundle(segment, id, resolveVersion(), time.Now())
	if err != nil {
		return nil, err
	}
	doc.Subject = subject
	return doc, nil
}

// sparseReceipt builds the tree receipt: only this run's own entries, each proved to belong to the
// whole chain by an audit path. Nothing about the entries around them travels, which is what lets a
// receipt be handed to an outside auditor on an install that runs other people's work.
func sparseReceipt(cmd *cobra.Command, entries []*audit.Entry, runID string,
	creationSeq, outcomeSeq int64, id audit.Identity,
	subject audit.BundleSubject) (*audit.Bundle, error) {
	// The run's own entries are its creation, whose receipt it carries, and every later entry whose
	// path names it: an approval, a rejection, a cancellation, and its outcome.
	disclose := map[int64]bool{creationSeq: true, outcomeSeq: true}
	for _, e := range entries {
		if e.Seq > creationSeq && strings.Contains(e.Path, runID) {
			disclose[e.Seq] = true
		}
	}
	doc, err := audit.BuildTreeBundle(entries, disclose, id, resolveVersion(), subject, time.Now())
	if err != nil {
		return nil, err
	}
	if receiptFrom > 0 {
		if err := doc.AttachConsistency(entries, receiptFrom, id); err != nil {
			return nil, err
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Proving the log only appended since size %d.\n", receiptFrom)
	}
	return doc, nil
}
