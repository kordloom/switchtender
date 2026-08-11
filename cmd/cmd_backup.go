package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/backup"
)

var (
	// backupDB is the --db value for the backup command.
	backupDB string
	// backupOut is the --out file path for the backup command; empty writes to stdout.
	backupOut string
	// restoreDB is the --db value for the restore command.
	restoreDB string
	// restoreIn is the --in file path for the restore command; empty reads from stdin.
	restoreIn string
)

// backupCmd writes an encrypted snapshot of the control plane.
var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Write an encrypted backup of credentials, projects, templates, inventories, schedules, triggers, tokens, and access.",
	Long: "Write an encrypted, portable backup of the control-plane configuration and secrets. The whole " +
		"file is sealed with the deployment encryption key, so it stays confidential and tamper-evident, and " +
		"it restores into either the SQLite or the PostgreSQL backend. Run history and the audit chain are " +
		"not included; the audit chain has its own signed export.",
	SilenceUsage: true,
	RunE:         runBackup,
}

// restoreCmd reads a backup and upserts its objects into the store.
var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore an encrypted backup, upserting its objects by id.",
	Long: "Read a backup written by the backup command and upsert its objects into the store by id. It needs " +
		"the same encryption key the backup was written with, and it never deletes objects absent from the file.",
	SilenceUsage: true,
	RunE:         runRestore,
}

// init registers the backup and restore commands and their flags.
func init() {
	backupCmd.Flags().StringVar(&backupDB, "db", defaultDBPath, "SQLite file path, or a postgres:// DSN, to back up.")
	backupCmd.Flags().StringVar(&backupOut, "out", "", "File to write the backup to. Defaults to stdout.")
	restoreCmd.Flags().StringVar(&restoreDB, "db", defaultDBPath, "SQLite file path, or a postgres:// DSN, to restore into.")
	restoreCmd.Flags().StringVar(&restoreIn, "in", "", "Backup file to read. Defaults to stdin.")
	rootCmd.AddCommand(backupCmd, restoreCmd)
}

// backupStores wires the backup store set from an open database bundle.
func backupStores(bundle storeBundle) backup.Stores {
	return backup.Stores{
		Credentials:      bundle.Credentials(),
		Projects:         bundle.Projects(),
		Templates:        bundle.Templates(),
		Inventories:      bundle.Inventories(),
		InventorySources: bundle.InventorySources(),
		Schedules:        bundle.Schedules(),
		Triggers:         bundle.Triggers(),
		Users:            bundle.Users(),
		Tokens:           bundle.Tokens(),
		Teams:            bundle.Teams(),
		Orgs:             bundle.Orgs(),
		Grants:           bundle.Grants(),
		CredentialTypes:  bundle.CredentialTypes(),
		Policies:         bundle.Policies(),
	}
}

// runBackup opens the store, writes the sealed snapshot to the output, and reports the counts. When
// no output path is given the backup goes to stdout and the counts go to stderr, so a piped backup
// stays clean.
func runBackup(cmd *cobra.Command, _ []string) error {
	bundle, err := openBundle(backupDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	sealer := newSealerFromEnv(zap.NewNop())

	if backupOut == "" {
		sum, err := backup.Write(cmd.Context(), backupStores(bundle), sealer, os.Stdout)
		if err != nil {
			return err
		}
		reportBackup(sum)
		return nil
	}

	// Write to a temp file in the destination directory and rename, so a partial or failed write never
	// replaces a good backup. The file is created 0600 because it holds sealed secrets.
	tmp, err := os.CreateTemp(filepath.Dir(backupOut), ".switchtender-backup-*")
	if err != nil {
		return fmt.Errorf("create backup file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure backup file: %w", err)
	}
	sum, err := backup.Write(cmd.Context(), backupStores(bundle), sealer, tmp)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write backup file: %w", err)
	}
	if err := os.Rename(tmpName, backupOut); err != nil {
		return fmt.Errorf("finalize backup file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Wrote backup to %s\n", backupOut)
	reportBackup(sum)
	return nil
}

// runRestore reads a backup from the input and upserts its objects into the store.
func runRestore(cmd *cobra.Command, _ []string) error {
	bundle, err := openBundle(restoreDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	in := os.Stdin
	if restoreIn != "" {
		f, err := os.Open(restoreIn)
		if err != nil {
			return fmt.Errorf("open backup file: %w", err)
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	// Recorded before the restore runs, and recorded whatever the outcome is. A restore rewrites
	// accounts, roles, and grants wholesale, which is the change most worth auditing in the whole
	// product, and it was the one CLI command exempt from recording. The exemption rested on the
	// idea that the file brings its own chain back with it, and it does not: the audit trail is
	// deliberately outside the backup, and a restore is a merge into a live install that keeps its
	// existing chain. So an account could be flipped to admin and a grant added while the chain
	// stayed byte-identical and went on verifying.
	if err := recordCLI(cmd.Context(), bundle.Audits(), "/cli/restore"); err != nil {
		return err
	}
	sum, err := backup.Read(cmd.Context(), backupStores(bundle), newSealerFromEnv(zap.NewNop()), in)
	// Record what the restore drew from, once it is known. sum.CreatedAt is set as the file decrypts
	// and validates, before anything is written, so it is non-zero exactly when data may have been
	// applied and zero when a bad key or format meant nothing was touched. The pre-restore guard entry
	// proved the trail is writable, so a failure to record this provenance is a real fault and is
	// returned even when the restore itself succeeded.
	if !sum.CreatedAt.IsZero() {
		if perr := recordRestoreProvenance(cmd.Context(), bundle.Audits(), sum); perr != nil {
			return perr
		}
	}
	if err != nil {
		// A restore is not atomic across stores, so a failure can still have written some of the
		// file. The counts say how far it got, which is the difference between an operator who
		// knows to look and one who was told nothing happened.
		fmt.Fprintln(os.Stderr, "Restore failed partway. What it had already written:")
		reportBackup(sum)
		return err
	}
	fmt.Fprintln(os.Stderr, "Restore complete.")
	reportBackup(sum)
	return nil
}

// recordRestoreProvenance records, after a restore, what it drew from: the instant the source backup
// was taken and the per-kind counts the Summary already holds. The pre-restore guard entry cannot
// carry these because they are known only once the file is read. The Summary is a struct of times and
// counts with no restored object in it, so the content digest it commits exposes nothing sensitive.
// The source instant also rides in the path so a reader sees it without opening the digest.
func recordRestoreProvenance(ctx context.Context, audits audit.Store, sum backup.Summary) error {
	body, err := json.Marshal(sum)
	if err != nil {
		return fmt.Errorf("encode restore provenance: %w", err)
	}
	path := fmt.Sprintf("/cli/restore?taken_at=%s", sum.CreatedAt.UTC().Format(time.RFC3339))
	return recordCLIChange(ctx, audits, path, body)
}

// reportBackup writes the object counts to stderr so they never mix with a backup written to stdout.
func reportBackup(s backup.Summary) {
	fmt.Fprintf(os.Stderr,
		"credentials %d, projects %d, templates %d, inventories %d, inventory sources %d, "+
			"schedules %d, triggers %d, users %d, tokens %d, teams %d, orgs %d, grants %d\n",
		s.Credentials, s.Projects, s.Templates, s.Inventories, s.InventorySources,
		s.Schedules, s.Triggers, s.Users, s.Tokens, s.Teams, s.Orgs, s.Grants)
}
