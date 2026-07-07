package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dcadolph/yardmaster/internal/importer"
)

// importDB holds the value of the import --db flag.
var importDB string

// importApply holds the value of the import --apply flag.
var importApply bool

// mapFunc maps an export document into a plan of Yardmaster objects.
type mapFunc func(data []byte, now time.Time) (*importer.Plan, error)

// importCmd groups the migration importers.
var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import AWX or Semaphore exports into Yardmaster.",
}

// importAWXCmd imports an awx export document.
var importAWXCmd = &cobra.Command{
	Use:   "awx <export.json>",
	Short: "Import an awx export into Yardmaster.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImport(cmd, args[0], importer.FromAWX)
	},
}

// importSemaphoreCmd imports a Semaphore export document.
var importSemaphoreCmd = &cobra.Command{
	Use:   "semaphore <export.json>",
	Short: "Import a Semaphore export into Yardmaster.",
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
		fmt.Println("\nRun again with --apply to create these objects.")
		return nil
	}
	created, err := applyPlan(cmd.Context(), plan)
	if err != nil {
		return err
	}
	fmt.Printf("\nCreated %d objects. Re-enter credential secrets before running templates that need them.\n", created)
	return nil
}

// reportPlan writes a human-readable summary of what an import will create to stdout.
func reportPlan(plan *importer.Plan) {
	fmt.Println("Import plan:")
	fmt.Printf("  Projects:    %d\n", len(plan.Projects))
	for _, p := range plan.Projects {
		fmt.Printf("    - %s (%s @ %s)\n", p.Name, p.RepoURL, branchOrDefault(p.Branch))
	}
	fmt.Printf("  Inventories: %d\n", len(plan.Inventories))
	for _, inv := range plan.Inventories {
		fmt.Printf("    - %s\n", inv.Name)
	}
	fmt.Printf("  Credentials: %d (secrets must be re-entered)\n", len(plan.Credentials))
	for _, c := range plan.Credentials {
		fmt.Printf("    - %s (%s)\n", c.Name, c.Kind)
	}
	fmt.Printf("  Templates:   %d\n", len(plan.Templates))
	for _, t := range plan.Templates {
		fmt.Printf("    - %s\n", t.Name)
	}
	fmt.Printf("  Schedules:   %d\n", len(plan.Schedules))
	for _, s := range plan.Schedules {
		fmt.Printf("    - %s (%s)\n", s.Name, s.Cron)
	}
	if len(plan.Warnings) > 0 {
		fmt.Printf("  Warnings (%d):\n", len(plan.Warnings))
		for _, w := range plan.Warnings {
			fmt.Printf("    - %s\n", w)
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

	created := 0
	for _, p := range plan.Projects {
		if err := bundle.Projects().Save(ctx, p); err != nil {
			return created, fmt.Errorf("save project %q: %w", p.Name, err)
		}
		created++
	}
	for _, inv := range plan.Inventories {
		if err := bundle.Inventories().Save(ctx, inv); err != nil {
			return created, fmt.Errorf("save inventory %q: %w", inv.Name, err)
		}
		created++
	}
	for _, c := range plan.Credentials {
		if err := bundle.Credentials().Save(ctx, c); err != nil {
			return created, fmt.Errorf("save credential %q: %w", c.Name, err)
		}
		created++
	}
	for _, t := range plan.Templates {
		if err := bundle.Templates().Save(ctx, t); err != nil {
			return created, fmt.Errorf("save template %q: %w", t.Name, err)
		}
		created++
	}
	for _, s := range plan.Schedules {
		if err := bundle.Schedules().Save(ctx, s); err != nil {
			return created, fmt.Errorf("save schedule %q: %w", s.Name, err)
		}
		created++
	}
	return created, nil
}

// branchOrDefault names a branch for display, calling out the remote default when unset.
func branchOrDefault(branch string) string {
	if branch == "" {
		return "default branch"
	}
	return branch
}
