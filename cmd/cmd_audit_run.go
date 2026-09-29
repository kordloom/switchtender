package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
)

var (
	// dossierDB is the --db value for the audit run command.
	dossierDB string
	// dossierOut is the --out value; empty writes to standard output.
	dossierOut string
)

// auditRunCmd emits one run's evidence dossier.
var auditRunCmd = &cobra.Command{
	Use:   "run <id>",
	Short: "Emit one run's evidence dossier as a self-contained HTML document.",
	Long: `Emit one run's evidence dossier.

The dossier answers an auditor's sample request in one document: what was run, who asked for it,
who approved or rejected it, what happened on each host, the chain entries that recorded each
decision, and the anchors that fix that history outside this install. Every chain entry carries a
seq:link receipt redeemable with "switchtender audit receipt", so the document's claims are
checkable without trusting the server that produced it.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runAuditRunDossier,
}

// init registers the audit run command and its flags.
func init() {
	auditRunCmd.Flags().StringVar(&dossierDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, to read the run and its chain from.")
	auditRunCmd.Flags().StringVar(&dossierOut, "out", "",
		"Write the dossier here instead of to standard output.")
	auditCmd.AddCommand(auditRunCmd)
}

// runAuditRunDossier collects one run's evidence and writes the rendered dossier.
func runAuditRunDossier(cmd *cobra.Command, args []string) error {
	store, err := openBundle(dossierDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// The install identity binds the tree profile's leaves, so a tree anchor cannot be checked
	// without it.
	id, err := audit.LoadIdentityForStore(dossierDB, identityDir(dossierDB))
	if err != nil {
		return err
	}
	in, err := dossier.Collect(cmd.Context(), store.Runs(), store.Audits(), id.InstallID, args[0],
		time.Now())
	if errors.Is(err, run.ErrNotFound) {
		return fmt.Errorf("run %s is not in this database", args[0])
	}
	if err != nil {
		return fmt.Errorf("collect evidence: %w", err)
	}
	doc, err := dossier.Render(in)
	if err != nil {
		return fmt.Errorf("render dossier: %w", err)
	}
	if dossierOut == "" {
		_, err = os.Stdout.Write(doc)
		return err
	}
	if err := os.WriteFile(dossierOut, doc, 0o600); err != nil {
		return fmt.Errorf("write dossier: %w", err)
	}
	fmt.Fprintln(os.Stderr, "wrote", dossierOut)
	return nil
}
