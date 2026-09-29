package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/jsonutil"
)

var (
	// receiptDB is the database holding the chain to redeem against.
	receiptDB = defaultDBPath
	// receiptPretty indents the printed result.
	receiptPretty bool
)

// auditReceiptCmd redeems a receipt against the stored chain.
var auditReceiptCmd = &cobra.Command{
	Use:   "receipt <seq:link>",
	Short: "Check that the audit chain still contains a receipt the server issued.",
	Long: `Check that the audit chain still contains a receipt the server issued.

Every mutation returns an Audit-Receipt header naming the chain position it was recorded at. Keep
those receipts. This redeems one. It confirms the chain holds that exact link at that exact
sequence, and that the chain around it verifies.

This is what a hash chain alone cannot give you. A chain proves that what it holds was not altered.
It cannot prove nothing is missing, because the same process decides both what happens and what gets
written down. A receipt you hold moves that from the server's word to your evidence. A server that
omitted the entry cannot produce a chain containing your receipt.`,
	Args: cobra.ExactArgs(1),
	RunE: runAuditReceipt,
}

// init registers the receipt command and its flags.
func init() {
	auditReceiptCmd.Flags().StringVar(&receiptDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, holding the chain to redeem against.")
	auditReceiptCmd.Flags().BoolVar(&receiptPretty, "pretty", false, "Indent the printed result.")
	auditCmd.AddCommand(auditReceiptCmd)
}

// runAuditReceipt redeems one receipt against the stored chain.
func runAuditReceipt(cmd *cobra.Command, args []string) error {
	seq, link, err := parseReceipt(args[0])
	if err != nil {
		return err
	}
	store, err := openBundle(receiptDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	chain, err := store.Audits().Chain(cmd.Context())
	if err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	// A receipt redeemed against a chain that does not verify proves nothing, so the chain is
	// checked before the receipt is looked for in it.
	if ok, brokeAt := audit.Verify(chain); !ok {
		return fmt.Errorf("the chain does not verify at entry %d, so no receipt can be redeemed "+
			"against it; GET /v1/audit/verify reports where", brokeAt)
	}
	// The anchors are consulted too. A receipt is what somebody outside this install holds, and the
	// question they are really asking is whether the record still contains what they were told it
	// did. Hashes alone cannot answer that: dropping entries off the end leaves a chain that still
	// verifies, so a redemption could confirm their entry while entries that provably existed were
	// gone. That is the exact case anchoring exists for.
	if anchors, ok := store.Audits().(audit.AnchorStore); ok {
		recorded, aerr := anchors.Anchors(cmd.Context(), 0)
		if aerr != nil {
			return fmt.Errorf("read anchors: %w", aerr)
		}
		// The install identity binds the tree profile's leaves, so a tree anchor cannot be checked
		// without it.
		id, ierr := audit.LoadIdentity(identityDir(receiptDB))
		if ierr != nil {
			return ierr
		}
		if reached, results := audit.CheckAnchors(chain, recorded, id.InstallID); !reached {
			for _, res := range results {
				if !res.Reached {
					fmt.Fprintln(os.Stderr, "anchor "+res.Anchor.ID+": "+res.Problem)
				}
			}
			return fmt.Errorf("this chain no longer satisfies an anchor recorded over it, so a " +
				"receipt redeemed against it proves nothing about what else is missing")
		}
	}
	for _, e := range chain {
		if e.Seq != seq {
			continue
		}
		if e.Hash != link {
			return fmt.Errorf("the chain holds a different entry at sequence %d: the receipt names "+
				"link %s and the chain has %s, so this is not the history the receipt came from",
				seq, link, e.Hash)
		}
		out, merr := jsonutil.Marshal(map[string]any{
			"redeemed": true, "seq": e.Seq, "link": e.Hash,
			"at":    e.At.UTC().Format("2006-01-02T15:04:05.000000Z"),
			"actor": e.Actor, "method": e.Method, "path": e.Path,
		}, receiptPretty)
		if merr != nil {
			return merr
		}
		fmt.Println(string(out))
		return nil
	}
	return fmt.Errorf("the chain has no entry at sequence %d, so this receipt is not in it; the "+
		"record is missing something that was reported as recorded", seq)
}

// parseReceipt splits the "seq:link" form the Audit-Receipt header carries.
func parseReceipt(v string) (int64, string, error) {
	seqPart, link, found := strings.Cut(v, ":")
	if !found {
		return 0, "", fmt.Errorf("a receipt looks like seq:link, for example 41:9f2c")
	}
	seq, err := strconv.ParseInt(seqPart, 10, 64)
	if err != nil || seq < 1 {
		return 0, "", fmt.Errorf("receipt sequence %q is not a chain position", seqPart)
	}
	if link == "" {
		return 0, "", fmt.Errorf("receipt is missing its link")
	}
	return seq, link, nil
}
