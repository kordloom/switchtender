package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/importer"
)

// importDB holds the value of the import --db flag.
var importDB string

// importApply holds the value of the import --apply flag.
var importApply bool

// mapFunc maps an export document into a plan of SwitchTender objects.
type mapFunc func(data []byte, now time.Time) (*importer.Plan, error)

// importCmd groups the migration importers.
var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import AWX or Semaphore exports into SwitchTender.",
}

// importAWXCmd imports an awx export document.
var importAWXCmd = &cobra.Command{
	Use:   "awx <export.json>",
	Short: "Import an awx export into SwitchTender.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImport(cmd, args[0], importer.FromAWX)
	},
}

// importSemaphoreCmd imports a Semaphore export document.
var importSemaphoreCmd = &cobra.Command{
	Use:   "semaphore <export.json>",
	Short: "Import a Semaphore export into SwitchTender.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImport(cmd, args[0], importer.FromSemaphore)
	},
}

// init registers the import commands and their flags.
func init() {
	for _, c := range []*cobra.Command{importAWXCmd, importSemaphoreCmd} {
		c.Flags().StringVar(&importDB, "db", defaultDBPath,
			"SQLite file path, or a postgres:// DSN, to write into with --apply.")
		c.Flags().BoolVar(&importApply, "apply", false,
			"Create the objects. Without it the import only reports what it would create.")
		importCmd.AddCommand(c)
	}
}

// runImport reads an export, maps it to a plan, reports the plan, and applies it when --apply is set.
func runImport(cmd *cobra.Command, path string, mapper mapFunc) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	plan, err := mapper(data, time.Now())
	if err != nil {
		return err
	}

	reportPlan(plan)
	if !importApply {
		fmt.Fprintln(os.Stderr, "\nRun again with --apply to create these objects.")
		return nil
	}
	created, err := applyPlan(cmd.Context(), plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"\nCreated %d objects. Re-enter credential secrets before running templates that need them.\n",
		created)
	return nil
}

// reportPlan writes a human-readable summary of what an import will create to stdout.
func reportPlan(plan *importer.Plan) {
	fmt.Fprintln(os.Stderr, "Import plan:")
	fmt.Fprintf(os.Stderr, "  Projects:    %d\n", len(plan.Projects))
	for _, p := range plan.Projects {
		fmt.Fprintf(os.Stderr, "    - %s (%s @ %s)\n", p.Name, p.RepoURL, branchOrDefault(p.Branch))
	}
	fmt.Fprintf(os.Stderr, "  Inventories: %d\n", len(plan.Inventories))
	for _, inv := range plan.Inventories {
		fmt.Fprintf(os.Stderr, "    - %s\n", inv.Name)
		// The content is shown, not just the name. An inventory is the list of machines a play
		// reaches and the variables it reaches them with, assembled from somebody else's export, so
		// a review that sees only a name is a review of nothing.
		for _, line := range strings.Split(strings.TrimRight(inv.Content, "\n"), "\n") {
			fmt.Fprintf(os.Stderr, "        %s\n", line)
		}
	}
	fmt.Fprintf(os.Stderr, "  Sources:     %d\n", len(plan.Sources))
	for _, s := range plan.Sources {
		fmt.Fprintf(os.Stderr, "    - %s (%s)\n", s.Name, s.Source)
	}
	fmt.Fprintf(os.Stderr, "  Credentials: %d (secrets must be re-entered)\n", len(plan.Credentials))
	for _, c := range plan.Credentials {
		fmt.Fprintf(os.Stderr, "    - %s (%s)\n", c.Name, c.Kind)
	}
	fmt.Fprintf(os.Stderr, "  Templates:   %d\n", len(plan.Templates))
	for _, t := range plan.Templates {
		fmt.Fprintf(os.Stderr, "    - %s\n", t.Name)
	}
	fmt.Fprintf(os.Stderr, "  Schedules:   %d\n", len(plan.Schedules))
	for _, s := range plan.Schedules {
		fmt.Fprintf(os.Stderr, "    - %s (%s)\n", s.Name, s.Cron)
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintf(os.Stderr, "  Warnings (%d):\n", len(plan.Warnings))
		for _, w := range plan.Warnings {
			fmt.Fprintf(os.Stderr, "    - %s\n", w)
		}
		if n := plan.Suppressed(); n > 0 {
			fmt.Fprintf(os.Stderr, "    (%d more not listed)\n", n)
		}
	}
}

// applyPlan persists a plan through the stores in dependency order and returns how many objects were
// created.
func applyPlan(ctx context.Context, plan *importer.Plan) (int, error) {
	bundle, err := openBundle(importDB)
	if err != nil {
		return 0, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	// One entry for the import as a whole. It creates many objects, and a chain that recorded each
	// separately would bury the fact that they arrived together from one export.
	if err := recordCLI(ctx, bundle.Audits(), "/cli/import/apply"); err != nil {
		return 0, err
	}
	return plan.Apply(ctx, importer.ApplyStores{
		Projects: bundle.Projects(), Inventories: bundle.Inventories(),
		Sources:     bundle.InventorySources(),
		Credentials: bundle.Credentials(), Templates: bundle.Templates(),
		Schedules: bundle.Schedules(),
	})
}

// branchOrDefault names a branch for display, calling out the remote default when unset.
func branchOrDefault(branch string) string {
	if branch == "" {
		return "default branch"
	}
	return branch
}
