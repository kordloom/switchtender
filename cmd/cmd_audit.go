package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/dossier"
)

// auditCmd groups audit trail tools.
var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit trail tools.",
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
	Use:   "report",
	Short: "Render the period change register as a self-contained HTML evidence report.",
	Long: `Render the period's change register.

Read the database and render every change with who asked for it, the chain-recorded decision over
it, its risk grade, and how it ended. That is the change-management evidence a SOC 2 CC8.1 or
ISO/IEC 27001 A.8.32 review samples from. The period defaults to the last 90 days; --from and --to
take a date or an RFC 3339 time.

To prove the chain itself to a third party, emit a signed bundle with audit bundle and have them
check it with the open loomseal verifier, on the command line or in a browser.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runAuditReport,
}

// init registers the audit commands and flags.
func init() {
	auditReportCmd.Flags().StringVar(&auditReportOut, "out", "",
		"File to write the HTML report to. Defaults to stdout.")
	auditReportCmd.Flags().StringVar(&auditReportDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, the change register reads from.")
	auditReportCmd.Flags().StringVar(&auditReportFrom, "from", "",
		"Change register period start, a date or RFC 3339 time. Defaults to 90 days before --to.")
	auditReportCmd.Flags().StringVar(&auditReportTo, "to", "",
		"Change register period end, exclusive. Defaults to now.")
	auditCmd.AddCommand(auditReportCmd)
	rootCmd.AddCommand(auditCmd)
}

// runAuditReport renders the period's change register from the database.
func runAuditReport(cmd *cobra.Command, _ []string) error {
	return runChangeRegister(cmd)
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
	// The install identity binds the tree profile's leaves, so a tree anchor cannot be checked
	// without it.
	id, err := loadProducerIdentity(auditReportDB)
	if err != nil {
		return err
	}
	in, err := dossier.CollectRegister(cmd.Context(), store.Runs(), store.Audits(), id.InstallID,
		from, to, time.Now(), dossier.MaxRegisterRuns)
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
