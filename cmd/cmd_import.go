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
	Short: "Import AWX, Semaphore, Rundeck, Jenkins, or crontab definitions into SwitchTender.",
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

// importCronInventory is the inventory imported crontab schedules target, since a crontab names no
// host. importCronSystem selects the six-field /etc/crontab form with a user column.
var (
	importCronInventory string
	importCronSystem    bool
)

// importRundeckInventory is the inventory imported Rundeck templates target, since Rundeck
// dispatches by node filter rather than by inventory file.
var (
	importRundeckInventory string
)

// importJenkinsInventory is the inventory imported Jenkins templates target, since Jenkins picks an
// agent by label rather than naming hosts.
var (
	importJenkinsInventory string
)

// importCronCmd imports a crontab into governed schedules.
var importCronCmd = &cobra.Command{
	Use:   "cron <crontab-file>",
	Short: "Import a crontab into governed schedules.",
	Long: `Import a crontab into governed schedules.

Each job line becomes a scheduled bash run, so an untracked cron job turns into one that is audited,
held for approval where a policy covers it, and tracked in host history like every other change. A
crontab names no target host, so pass --inventory to say where the jobs run. Read /etc/crontab and
the system tabs with --system, which parses the user column and reports it. Comments, environment
assignments, and @reboot lines are reported and skipped rather than guessed at.

Without --apply the import only reports what it would create.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImport(cmd, args[0], importer.FromCron(importCronInventory, importCronSystem))
	},
}

// importRundeckCmd imports a Rundeck job export.
var importRundeckCmd = &cobra.Command{
	Use:   "rundeck <jobs.yaml>",
	Short: "Import a Rundeck job export into SwitchTender.",
	Long: `Import a Rundeck job export into SwitchTender.

Each job becomes a Bash template carrying its step sequence, its options become a survey, and its
schedule becomes a cron schedule with the Quartz weekday numbering converted. Rundeck dispatches by
node filter rather than by inventory file, so pass --inventory to say which hosts these jobs target.

A secure option is never imported as a survey field, because that would turn a password prompt into
a value stored in plain text on every run. Store those as credentials instead; the report names each
one. Job references, plugin steps, and remote script URLs are reported rather than guessed at.

Without --apply the import only reports what it would create.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImport(cmd, args[0], importer.FromRundeck(importRundeckInventory))
	},
}

// importJenkinsCmd imports Jenkins freestyle jobs.
var importJenkinsCmd = &cobra.Command{
	Use:   "jenkins <JENKINS_HOME|jobs-dir|config.xml>",
	Short: "Import Jenkins freestyle jobs into SwitchTender.",
	Long: `Import Jenkins freestyle jobs into SwitchTender.

Point this at a JENKINS_HOME, at its jobs directory, or at a single job's config.xml. Folders are
followed and each job keeps its full name. A job's build steps become one Bash template, its
parameters become a survey, and every line of its build trigger becomes a schedule, with Jenkins H
notation resolved to concrete times and the weekday renumbered where the two disagree.

Only freestyle jobs are imported. A Pipeline job is a Groovy program with no honest mechanical
translation into a template, so it is named and skipped rather than half-imported. A poll trigger is
also skipped: it builds only when the repository changed, so importing it as a schedule would run
the job every interval whether anything changed or not.

Password parameters and remote trigger tokens are never imported, because both would turn a secret
into a stored plaintext value. Jenkins picks an agent by label rather than naming hosts, so pass
--inventory to say which machines these jobs run against.

Without --apply the import only reports what it would create.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bundle, err := importer.JenkinsBundle(args[0])
		if err != nil {
			return err
		}
		if line := importer.JenkinsFoundLine(importer.JenkinsJobNames(bundle)); line != "" {
			fmt.Fprintln(os.Stderr, line)
		}
		return runImportData(cmd, bundle, importer.FromJenkins(importJenkinsInventory))
	},
}

// init registers the import commands and their flags.
func init() {
	for _, c := range []*cobra.Command{
		importAWXCmd, importSemaphoreCmd, importCronCmd, importRundeckCmd, importJenkinsCmd,
	} {
		c.Flags().StringVar(&importDB, "db", defaultDBPath,
			"SQLite file path, or a postgres:// DSN, to write into with --apply.")
		c.Flags().BoolVar(&importApply, "apply", false,
			"Create the objects. Without it the import only reports what it would create.")
		importCmd.AddCommand(c)
	}
	importCronCmd.Flags().StringVar(&importCronInventory, "inventory", "",
		"Inventory the imported schedules target, since a crontab names no host.")
	importCronCmd.Flags().BoolVar(&importCronSystem, "system", false,
		"Parse the six-field /etc/crontab form, whose user column sits before the command.")
	importRundeckCmd.Flags().StringVar(&importRundeckInventory, "inventory", "",
		"Inventory the imported templates target, since Rundeck dispatches by node filter.")
	importJenkinsCmd.Flags().StringVar(&importJenkinsInventory, "inventory", "",
		"Inventory the imported templates target, since Jenkins picks an agent by label.")
}

// runImport reads an export, maps it to a plan, reports the plan, and applies it when --apply is set.
func runImport(cmd *cobra.Command, path string, mapper mapFunc) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	return runImportData(cmd, data, mapper)
}

// runImportData maps an export document already in hand, reports the plan, and applies it when
// --apply is set.
func runImportData(cmd *cobra.Command, data []byte, mapper mapFunc) error {
	plan, err := mapper(data, time.Now())
	if err != nil {
		return err
	}

	reportPlan(plan)
	if !importApply {
		fmt.Fprintln(os.Stderr, "\nRun again with --apply to create these objects.")
		return nil
	}
	before := len(plan.Warnings)
	created, err := applyPlan(cmd.Context(), plan)
	if err != nil {
		return err
	}
	// Apply raises its own warnings, most importantly that a template names an inventory this install
	// does not have and will be treated as a filesystem path. The plan was printed before apply ran,
	// so those lines had nowhere to appear and the operator learned about it from a failed run.
	if len(plan.Warnings) > before {
		fmt.Fprintf(os.Stderr, "\n  Warnings from the import (%d):\n", len(plan.Warnings)-before)
		for _, w := range plan.Warnings[before:] {
			fmt.Fprintf(os.Stderr, "    - %s\n", w)
		}
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
