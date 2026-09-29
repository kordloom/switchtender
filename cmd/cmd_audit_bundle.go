package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
)

var (
	// bundleDB is the --db value for the audit bundle command.
	bundleDB string
	// bundleOut is the --out value; empty writes to standard output.
	bundleOut string
	// bundleLimit caps how many of the newest entries the bundle carries.
	bundleLimit int
	// bundleKeyDir overrides where the producer identity is read from or created.
	bundleKeyDir string
)

// auditBundleCmd emits the audit chain as a signed LoomSeal bundle.
var auditBundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Emit the audit chain as a signed LoomSeal bundle anyone can verify offline.",
	Long: `Emit the audit chain as a signed LoomSeal bundle.

A bundle is a self-contained record. It carries the entries, the chain links, the public key that
signed it, and the signature, so a third party verifies it with an open verifier on a machine that
has never run SwitchTender and has no network access. Every link recomputes from the claims alone,
so nobody has to take our word for the history.

The signing key is created on first use and never leaves the install. Publish the fingerprint the
command prints so a relying party can pin it and know a bundle came from this install rather than
from someone who merely generated a key.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runAuditBundle,
}

// init registers the bundle command and its flags.
func init() {
	auditBundleCmd.Flags().StringVar(&bundleDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, to read the audit chain from.")
	auditBundleCmd.Flags().StringVar(&bundleOut, "out", "",
		"Write the bundle here instead of to standard output.")
	auditBundleCmd.Flags().IntVar(&bundleLimit, "limit", 0,
		"Carry only the newest N entries. The default carries the whole chain.")
	auditBundleCmd.Flags().StringVar(&bundleKeyDir, "key-dir", "",
		"Directory holding the producer signing key. Defaults to the database's directory.")
	auditCmd.AddCommand(auditBundleCmd)
}

// runAuditBundle reads the chain, assembles it into a bundle, signs it, and writes it out.
func runAuditBundle(cmd *cobra.Command, _ []string) error {
	store, err := openBundle(bundleDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Chain returns the whole chain oldest first, which is the order a bundle's claims must be in.
	// List is deliberately not used: it returns newest first and clamps a limit below one up to one,
	// so it would have produced a single claim in the wrong order.
	entries, err := store.Audits().Chain(cmd.Context())
	if err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("the audit chain is empty, there is nothing to bundle")
	}
	// The whole chain is held against the anchors before any window is applied. Checking the window
	// instead read a deliberate narrowing as lost history: --limit refused to run at all on an
	// anchored install, reporting that entries the anchor covers were missing when they were merely
	// outside the window. The workaround an operator would find is to delete the anchors, which is
	// the one action that makes real truncation invisible.
	full := entries
	if bundleLimit > 0 && len(entries) > bundleLimit {
		entries = entries[len(entries)-bundleLimit:]
	}

	id, err := audit.LoadIdentity(keyDir())
	if err != nil {
		return err
	}
	doc, err := audit.BuildBundle(entries, id, resolveVersion(), time.Now())
	if err != nil {
		return err
	}
	// Anchors go on before signing, so the signature covers them.
	//
	// Every anchor is read, not only those at or below the head. Asking for anchors up to the head
	// meant a chain shortened below its anchor filtered that anchor out of its own evidence: the
	// bundle came out clean, signed, and missing exactly the thing that disproved it. The anchors
	// are what a shortened chain fails, so the chain is held against all of them and a chain that
	// cannot reach one is refused rather than published.
	if anchors, ok := store.Audits().(audit.AnchorStore); ok {
		recorded, aerr := anchors.Anchors(cmd.Context(), 0)
		if aerr != nil {
			return fmt.Errorf("read anchors: %w", aerr)
		}
		if reachedAll, results := audit.CheckAnchors(full, recorded); !reachedAll {
			for _, res := range results {
				if !res.Reached {
					fmt.Fprintln(os.Stderr, "anchor "+res.Anchor.ID+": "+res.Problem)
				}
			}
			return fmt.Errorf("this chain does not satisfy every anchor recorded over it, so it " +
				"must not be published as one that does")
		}
		if n := doc.AttachAnchors(recorded); n > 0 {
			fmt.Fprintf(os.Stderr, "Attached %d anchor(s), so a verifier can see this chain has "+
				"not been shortened.\n", n)
		}
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		return err
	}
	signed = append(signed, '\n')

	if bundleOut == "" {
		_, err = os.Stdout.Write(signed)
		return err
	}
	if err := os.WriteFile(bundleOut, signed, 0o644); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	// The fingerprint goes to standard error so it never contaminates a bundle written to standard
	// output, and so a reader is told the one thing they must publish for the bundle to mean anything.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Wrote %s with %d entries.\nPublish this fingerprint so a verifier can pin it:\n  %s\n",
		bundleOut, len(entries), id.KeyID())
	return nil
}

// keyDir returns where the producer identity lives: the override when given, otherwise beside the
// database, which is already the directory an operator backs up and protects.
func keyDir() string {
	if bundleKeyDir != "" {
		return bundleKeyDir
	}
	dir := filepath.Dir(bundleDB)
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}
