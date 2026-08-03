package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/jsonutil"
)

const (
	// defaultTSA is a free, public RFC 3161 timestamp authority, so an anchor needs no account and
	// no infrastructure to create. Any authority works; this one makes the default path free.
	defaultTSA = "https://freetsa.org/tsr"
	// anchorTimeout bounds how long a timestamp authority has to answer.
	anchorTimeout = 30 * time.Second
)

var (
	// anchorDB is the database holding the chain to anchor.
	anchorDB = defaultDBPath
	// anchorType selects the anchor kind: rfc3161, git, or https.
	anchorType = audit.AnchorRFC3161
	// anchorRef locates the anchor: a timestamp authority URL, or the URL of a published head.
	anchorRef string
	// anchorPretty indents the printed anchor.
	anchorPretty bool
)

// auditAnchorCmd fixes the current chain head somewhere outside this install.
var auditAnchorCmd = &cobra.Command{
	Use:   "anchor",
	Short: "Fix the current audit chain head in a place this install cannot rewrite.",
	Long: `Fix the current audit chain head in a place this install cannot rewrite.

A hash chain proves nothing in it was altered. It cannot prove nothing was removed from the end,
because a prefix of a valid chain is itself a valid chain. Drop the last thousand entries and what
remains still verifies. An anchor closes that. Once a link is recorded outside this install, a chain
that no longer reaches it has visibly lost its tail.

The default asks a public RFC 3161 timestamp authority to sign the time it saw the head. That token
is embedded in every bundle built afterwards and is checked offline, with no network and no trust in
this install, by standard tooling such as openssl ts -verify.

Anchor on a schedule. An anchor bounds how much history can be dropped unnoticed to whatever
happened since the last one.`,
	Args: cobra.NoArgs,
	RunE: runAuditAnchor,
}

// init registers the anchor command and its flags.
func init() {
	auditAnchorCmd.Flags().StringVar(&anchorDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, holding the chain to anchor.")
	auditAnchorCmd.Flags().StringVar(&anchorType, "type", audit.AnchorRFC3161,
		"Anchor kind: rfc3161 for a signed timestamp, or git or https for one checked by fetching it.")
	auditAnchorCmd.Flags().StringVar(&anchorRef, "ref", "",
		"Timestamp authority URL for rfc3161, otherwise the URL a verifier fetches. Defaults to a public authority.")
	auditAnchorCmd.Flags().BoolVar(&anchorPretty, "pretty", false, "Indent the printed anchor.")
	auditCmd.AddCommand(auditAnchorCmd)
}

// runAuditAnchor records an anchor over the current chain head.
func runAuditAnchor(cmd *cobra.Command, _ []string) error {
	store, err := openBundle(anchorDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	anchors, ok := store.Audits().(audit.AnchorStore)
	if !ok {
		return fmt.Errorf("this store does not keep anchors")
	}
	chain, err := store.Audits().Chain(cmd.Context())
	if err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	if len(chain) == 0 {
		return fmt.Errorf("the audit chain is empty, there is nothing to anchor")
	}
	// Anchoring a chain that does not verify would fix a broken record in time and give it the
	// standing of an anchored one.
	if ok, brokeAt := audit.Verify(chain); !ok {
		return fmt.Errorf("the chain does not verify at entry %d, so it must not be anchored; "+
			"run audit verify to see where", brokeAt)
	}
	head := chain[len(chain)-1]

	ref := anchorRef
	if ref == "" && anchorType == audit.AnchorRFC3161 {
		ref = defaultTSA
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), anchorTimeout)
	defer cancel()
	a, err := audit.NewAnchor(ctx, &http.Client{Timeout: anchorTimeout},
		anchorType, ref, head.Seq, head.Hash, time.Now())
	if err != nil {
		return err
	}
	if err := anchors.SaveAnchor(cmd.Context(), a); err != nil {
		return fmt.Errorf("save anchor: %w", err)
	}
	out, err := jsonutil.Marshal(map[string]any{
		"id": a.ID, "type": a.Type, "seq": a.Seq, "link": a.Link,
		"at": a.At.UTC().Format(time.RFC3339), "ref": a.Ref, "has_proof": a.Proof != "",
	}, anchorPretty)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
