package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
)

// auditCmd groups audit trail tools.
var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit trail tools.",
}

// auditExpectKey holds the --pubkey value for audit verify.
var auditExpectKey string

// auditVerifyCmd verifies a signed audit export offline.
var auditVerifyCmd = &cobra.Command{
	Use:   "verify <export.json>",
	Short: "Verify an audit export offline. Recompute the chain and check the signature.",
	Args:  cobra.ExactArgs(1),
	// A failed verification is a real result, not a usage error, so keep the output clean.
	SilenceUsage: true,
	RunE:         runAuditVerify,
}

// auditKeygenCmd mints an ed25519 signing key for audit exports.
var auditKeygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate an ed25519 audit signing key. Set the seed as SWITCHTENDER_AUDIT_KEY.",
	RunE:  runAuditKeygen,
}

// auditReportOut holds the --out value for audit report.
var auditReportOut string

// Period register flags for audit report with no export argument.
var (
	// auditReportDB is the --db value the change register reads from.
	auditReportDB string
	// auditReportFrom and auditReportTo bound the register period, dates or RFC 3339 times.
	auditReportFrom string
	auditReportTo   string
)

// auditReportCmd renders a signed export into a shareable HTML evidence report.
var auditReportCmd = &cobra.Command{
	Use:   "report [export.json]",
	Short: "Render evidence: an export's report, or a period change register with --from and --to.",
	Long: `Render evidence.

With an export file, verify it offline and render it as a self-contained HTML evidence report.

Without one, read the database and render the period's change register: every change with who asked
for it, the chain-recorded decision over it, its risk grade, and how it ended. That is the
change-management evidence a SOC 2 CC8.1 or ISO/IEC 27001 A.8.32 review samples from. The period
defaults to the last 90 days; --from and --to take a date or an RFC 3339 time.`,
	Args: cobra.MaximumNArgs(1),
	// A broken or unsigned export still produces a report stating so, not a usage error.
	SilenceUsage: true,
	RunE:         runAuditReport,
}

// init registers the audit commands and flags.
func init() {
	auditVerifyCmd.Flags().StringVar(&auditExpectKey, "pubkey", "",
		"Expected signer public key in hex; verification fails when the export's key differs.")
	auditReportCmd.Flags().StringVar(&auditReportOut, "out", "",
		"File to write the HTML report to. Defaults to stdout.")
	auditReportCmd.Flags().StringVar(&auditReportDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, the change register reads from.")
	auditReportCmd.Flags().StringVar(&auditReportFrom, "from", "",
		"Change register period start, a date or RFC 3339 time. Defaults to 90 days before --to.")
	auditReportCmd.Flags().StringVar(&auditReportTo, "to", "",
		"Change register period end, exclusive. Defaults to now.")
	auditCmd.AddCommand(auditVerifyCmd, auditKeygenCmd, auditReportCmd)
	rootCmd.AddCommand(auditCmd)
}

// runAuditReport reads an export, verifies it, and writes a self-contained HTML evidence report an
// auditor or a customer can read without any tooling and re-verify against the export.
func runAuditReport(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return runChangeRegister(cmd)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	var exp audit.Export
	if err := json.Unmarshal(data, &exp); err != nil {
		return fmt.Errorf("parse export: %w", err)
	}
	html, err := audit.Report(&exp, time.Now())
	if err != nil {
		return err
	}
	if auditReportOut == "" {
		_, err := os.Stdout.Write(html)
		return err
	}
	if err := os.WriteFile(auditReportOut, html, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Wrote report to %s\n", auditReportOut)
	return nil
}

// runAuditVerify reads an export file, recomputes the chain, and checks the signature, so an auditor
// can prove the trail is intact without trusting the server that produced it.
func runAuditVerify(_ *cobra.Command, args []string) error {
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	var exp audit.Export
	if err := json.Unmarshal(data, &exp); err != nil {
		return fmt.Errorf("parse export: %w", err)
	}
	if auditExpectKey != "" && exp.PublicKey != auditExpectKey {
		return fmt.Errorf("public key mismatch: export signed by %q, expected %q", exp.PublicKey, auditExpectKey)
	}
	// The printed count comes from the file, so it is checked against what the file actually holds.
	// On a signed export the signature pins it; on an unsigned one nothing did, and the number an
	// operator reads is the one thing they use to notice entries are missing.
	if exp.Count != len(exp.Entries) {
		return fmt.Errorf("this export says it holds %d entries and holds %d, so it is not the "+
			"chain it claims to be", exp.Count, len(exp.Entries))
	}
	signed, err := audit.VerifyExport(&exp)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	fmt.Printf("chain intact: %d entries, head %s\n", exp.Count, shortHex(exp.HeadHash))
	if signed {
		fmt.Printf("signature valid: ed25519 public key %s\n", exp.PublicKey)
	} else {
		fmt.Println("signature: none (integrity proven, attribution not)")
	}
	return nil
}

// runAuditKeygen prints a fresh ed25519 seed and its public key.
func runAuditKeygen(_ *cobra.Command, _ []string) error {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	hexSeed := hex.EncodeToString(seed)
	signer, err := audit.NewSigner(hexSeed)
	if err != nil {
		return err
	}
	return printJSON(map[string]string{"seed": hexSeed, "public_key": signer.PublicKeyHex()})
}

// shortHex truncates a hex digest for display.
func shortHex(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// runChangeRegister reads the database and writes the period's change register.
func runChangeRegister(cmd *cobra.Command) error {
	to := time.Now()
	if auditReportTo != "" {
		parsed, err := parseReportTime(auditReportTo)
		if err != nil {
			return fmt.Errorf("parse --to: %w", err)
		}
		to = parsed
	}
	from := to.AddDate(0, 0, -90)
	if auditReportFrom != "" {
		parsed, err := parseReportTime(auditReportFrom)
		if err != nil {
			return fmt.Errorf("parse --from: %w", err)
		}
		from = parsed
	}
	if !from.Before(to) {
		return fmt.Errorf("--from %s does not precede --to %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	store, err := openBundle(auditReportDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	in, err := dossier.CollectRegister(cmd.Context(), store.Runs(), store.Audits(), from, to, time.Now())
	if err != nil {
		return fmt.Errorf("collect change register: %w", err)
	}
	doc, err := dossier.RenderRegister(in)
	if err != nil {
		return err
	}
	if auditReportOut == "" {
		_, err = os.Stdout.Write(doc)
		return err
	}
	if err := os.WriteFile(auditReportOut, doc, 0o600); err != nil {
		return fmt.Errorf("write register: %w", err)
	}
	fmt.Fprintln(os.Stderr, "wrote", auditReportOut)
	return nil
}

// parseReportTime accepts a date or an RFC 3339 time.
func parseReportTime(raw string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}
