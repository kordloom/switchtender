package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dcadolph/switchtender/internal/audit"
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

// init registers the audit commands and flags.
func init() {
	auditVerifyCmd.Flags().StringVar(&auditExpectKey, "pubkey", "",
		"Expected signer public key in hex; verification fails when the export's key differs.")
	auditCmd.AddCommand(auditVerifyCmd, auditKeygenCmd)
	rootCmd.AddCommand(auditCmd)
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
