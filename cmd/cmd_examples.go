package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/template"
)

// examplesDB is the database the starter templates are written into.
var examplesDB string

// examplesCmd seeds a few runnable starter templates so a fresh install has something to launch.
var examplesCmd = &cobra.Command{
	Use:   "examples",
	Short: "Add a few runnable starter templates to a fresh install.",
	Long: `Seed starter templates.

A new install opens with an empty templates list, which is a poor first look. This adds a handful of
templates that run with no project, inventory, or credential, so a first launch works on the spot:
they use the Bash tool and print or read something local. Delete them once you have your own.

Run it against the same database serve uses. It skips a template whose name is already present, so it
is safe to run twice.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runExamples,
}

// init registers the examples command and its flag.
func init() {
	examplesCmd.Flags().StringVar(&examplesDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, to write the templates into.")
	rootCmd.AddCommand(examplesCmd)
}

// starterTemplates are the seed templates. Each runs standalone under the Bash tool, so a launch
// needs nothing else set up first.
func starterTemplates(now time.Time) []*template.Template {
	return []*template.Template{
		{
			Name: "Hello, SwitchTender", Tool: "bash",
			Command:   "echo \"SwitchTender is running on $(hostname) at $(date -u +%FT%TZ)\"",
			CreatedAt: now,
		},
		{
			Name: "Show host uptime and load", Tool: "bash",
			Command:   "uptime && echo '---' && (cat /proc/loadavg 2>/dev/null || sysctl -n vm.loadavg 2>/dev/null)",
			CreatedAt: now,
		},
		{
			Name: "Disk usage summary", Tool: "bash",
			Command:   "df -h 2>/dev/null | sort -k5 -r | head -20",
			CreatedAt: now,
		},
		{
			Name: "Confirm before it runs", Tool: "bash",
			Command:         "echo 'This template asks for a review before every launch.'",
			ConfirmOnLaunch: true,
			CreatedAt:       now,
		},
	}
}

// runExamples writes the starter templates, skipping any whose name already exists.
func runExamples(cmd *cobra.Command, _ []string) error {
	// Seeding into a database that is not there creates one, silently, and then reports four
	// templates added while the server's own database stays empty. The templates page offers this
	// command with no --db, so a reader whose server runs with one, or who runs it from another
	// directory, gets four success lines and a page that never changes. Serve says the same thing
	// for the same reason.
	fresh := newDatabase(examplesDB)

	bundle, err := openBundle(examplesDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	added, err := seedExamples(cmd.Context(), bundle.Audits(), bundle.Templates(), time.Now())
	if err != nil {
		return err
	}
	if added == 0 {
		fmt.Fprintln(os.Stderr, "every starter template is already present; nothing added.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "added %d starter template(s). Launch one from the templates page or the API.\n", added)
	if fresh {
		fmt.Fprintf(os.Stderr,
			"these went into a new database at %s, which was not there before. If your server runs "+
				"with a different --db, it will not see them: rerun with the same --db the server "+
				"uses.\n", examplesDB)
	}
	return nil
}

// newDatabase reports whether the given store is a local file that does not exist yet, so a command
// about to create one can say so. A Postgres DSN is never a fresh file.
func newDatabase(db string) bool {
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		return false
	}
	_, err := os.Stat(db)
	return errors.Is(err, os.ErrNotExist)
}

// seedExamples saves each starter template whose name is not already in the store, returning how many
// it added. It reports names to stderr as it goes and leaves an existing template of the same name
// untouched, so a second run is a no-op rather than a duplicate.
//
// Seeding writes into the same store the server serves, so it is an operator action like any other
// and is recorded in the audit chain before the first write. A change that leaves no entry is a
// silence an auditor cannot tell apart from tampering. A run that adds nothing records nothing.
func seedExamples(ctx context.Context, audits audit.Store, store template.Store,
	now time.Time) (int, error) {
	existing, err := store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list templates: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, t := range existing {
		have[t.Name] = true
	}

	var missing []*template.Template
	for _, t := range starterTemplates(now) {
		if !have[t.Name] {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	if err := recordCLI(ctx, audits, "/cli/examples"); err != nil {
		return 0, err
	}

	added := 0
	for _, t := range missing {
		t.ID = template.NewID()
		if err := store.Save(ctx, t); err != nil {
			return added, fmt.Errorf("save %q: %w", t.Name, err)
		}
		fmt.Fprintf(os.Stderr, "added template %q\n", t.Name)
		added++
	}
	return added, nil
}
