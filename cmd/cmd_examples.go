package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

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
	bundle, err := openBundle(examplesDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	added, err := seedExamples(cmd.Context(), bundle.Templates(), time.Now())
	if err != nil {
		return err
	}
	if added == 0 {
		fmt.Fprintln(os.Stderr, "every starter template is already present; nothing added.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "added %d starter template(s). Launch one from the templates page or the API.\n", added)
	return nil
}

// seedExamples saves each starter template whose name is not already in the store, returning how many
// it added. It reports names to stderr as it goes and leaves an existing template of the same name
// untouched, so a second run is a no-op rather than a duplicate.
func seedExamples(ctx context.Context, store template.Store, now time.Time) (int, error) {
	existing, err := store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list templates: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, t := range existing {
		have[t.Name] = true
	}

	added := 0
	for _, t := range starterTemplates(now) {
		if have[t.Name] {
			continue
		}
		t.ID = template.NewID()
		if err := store.Save(ctx, t); err != nil {
			return added, fmt.Errorf("save %q: %w", t.Name, err)
		}
		fmt.Fprintf(os.Stderr, "added template %q\n", t.Name)
		added++
	}
	return added, nil
}
