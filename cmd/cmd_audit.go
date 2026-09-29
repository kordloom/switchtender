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
	Short: "Verify an audit export offline: recompute the chain and check the signature.",
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

// auditReportCmd renders a signed export into a shareable HTML evidence report.
var auditReportCmd = &cobra.Command{
	Use:   "report <export.json>",
	Short: "Render an audit export into a self-contained HTML evidence report, verifying it offline.",
	Args:  cobra.ExactArgs(1),
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
	auditCmd.AddCommand(auditVerifyCmd, auditKeygenCmd, auditReportCmd)
	rootCmd.AddCommand(auditCmd)
}

// runAuditReport reads an export, verifies it, and writes a self-contained HTML evidence report an
// auditor or a customer can read without any tooling and re-verify against the export.
func runAuditReport(_ *cobra.Command, args []string) error {
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
