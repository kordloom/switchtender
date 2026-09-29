package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/importer"
)

// assessMappers is the format name to reader map, taken from the same functions import uses.
//
// Shared deliberately. An assessment that read an export differently from the import that follows
// it would be a document promising one thing and a tool doing another, which is the exact failure
// this whole feature exists to prevent somebody discovering in production.
var assessMappers = map[string]func([]byte, time.Time) (*importer.Plan, error){
	"awx":       importer.FromAWX,
	"semaphore": importer.FromSemaphore,
	"chef":      importer.FromChef,
	"puppet":    importer.FromPuppet,
}

// assessCmd reports what an export holds and what governing it would change, without writing
// anything and without needing a database.
var assessCmd = &cobra.Command{
	Use:   "assess <format> <export>",
	Short: "Report what an export holds and what governing it would change.",
	Long: "Assess an automation estate before importing any of it.\n\n" +
		"Reads an export and answers three questions: what is in there, what survives the move, " +
		"and what changes about how it is governed. Nothing is written and no database is " +
		"needed, so it can be run against an export from a system this has never touched.\n\n" +
		"The governance grades come from the same graders the product uses at run time, so a " +
		"number here cannot disagree with what a run would later say.\n\n" +
		"Formats: awx, semaphore, chef, puppet. An AWX-format export also covers Ansible " +
		"Automation Platform, Tower, and Ascender.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		format := strings.ToLower(args[0])
		mapper, ok := assessMappers[format]
		if !ok {
			names := make([]string, 0, len(assessMappers))
			for name := range assessMappers {
				names = append(names, name)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown format %q: expected one of %s",
				format, strings.Join(names, ", "))
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("read export: %w", err)
		}
		plan, err := mapper(data, time.Now())
		if err != nil {
			return err
		}
		printAssessment(cmd, format, args[1], plan.Assess())
		return nil
	},
}

// printAssessment renders the assessment as the document somebody forwards, rather than as a dump.
func printAssessment(cmd *cobra.Command, format, path string, a importer.Assessment) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "SwitchTender migration assessment\n")
	fmt.Fprintf(out, "  Source: %s export, %s\n\n", format, path)

	fmt.Fprintf(out, "What is in there\n")
	for _, c := range a.Report.Created {
		fmt.Fprintf(out, "  %-22s %6d\n", c.Kind, c.N)
	}
	fmt.Fprintf(out, "  %-22s %6d\n\n", "total", a.Report.CreatedTotal)

	fmt.Fprintf(out, "What survives the move\n")
	fmt.Fprintf(out, "  %-22s %6d\n", "comes across", a.Report.CreatedTotal)
	if a.Report.NeedsSecret > 0 {
		fmt.Fprintf(out, "  %-22s %6d   re-entered once; an export never carries secret values\n",
			"needs a secret", a.Report.NeedsSecret)
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "does not come across", len(a.Report.LeftOut))
	for _, w := range capList(a.Report.LeftOut, 6) {
		fmt.Fprintf(out, "      - %s\n", w)
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "worth reviewing", len(a.Report.NeedsReview))
	for _, w := range capList(a.Report.NeedsReview, 4) {
		fmt.Fprintf(out, "      - %s\n", w)
	}
	if a.Report.Suppressed > 0 {
		fmt.Fprintf(out, "  %d further warning(s) were not listed, because this export passed the "+
			"report's cap.\n", a.Report.Suppressed)
	}
	fmt.Fprintln(out)

	g := a.Governance
	fmt.Fprintf(out, "What changes about how it is governed\n")
	if g.Templates == 0 {
		fmt.Fprintf(out, "  This export holds no templates, so there is nothing here to gate. What\n"+
			"  it brings is the estate itself, which is what later runs are governed against.\n\n")
		return
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "templates graded", g.Templates)
	fmt.Fprintf(out, "  %-22s %6d   cannot be undone from here\n", "irreversible", len(g.Irreversible))
	for _, n := range capList(g.Irreversible, 6) {
		fmt.Fprintf(out, "      - %s\n", n)
	}
	fmt.Fprintf(out, "  %-22s %6d   undone only by doing more work\n", "costly", len(g.Costly))
	fmt.Fprintf(out, "  %-22s %6d   carry a destructive signal\n", "high risk", len(g.HighRisk))
	for _, n := range capList(g.HighRisk, 6) {
		fmt.Fprintf(out, "      - %s\n", n)
	}
	fmt.Fprintf(out, "  %-22s %6d   templates use a credential another template also uses\n",
		"shared credentials", len(g.SharedCredentials))
	fmt.Fprintf(out, "  %-22s %6d   target no stored inventory\n", "no inventory", len(g.NoInventory))
	fmt.Fprintln(out)

	if g.Unread > 0 {
		fmt.Fprintf(out, "  Grades come from what each template declares: its tool, command, limit,\n"+
			"  and variables. %d of them run an Ansible playbook whose text lives in a repository\n"+
			"  nothing has fetched yet. Reading a playbook can only raise a grade, never lower one,\n"+
			"  so every number above is a floor. The real estate is at least this destructive.\n\n",
			g.Unread)
	}

	// The one sentence this document exists to produce.
	fmt.Fprintf(out, "  Today any of these %d templates runs when somebody presses the button.\n",
		g.Templates)
	if g.WouldGate > 0 {
		fmt.Fprintf(out, "  One approval policy holding on an irreversible grade would stop %d of\n"+
			"  them until a second person agrees, and every run would leave a receipt that\n"+
			"  verifies without us.\n", g.WouldGate)
	} else {
		fmt.Fprintf(out, "  None of them grades irreversible, so an approval policy holding on that\n"+
			"  grade would stop none of them. A policy on risk or on tool is the one to write here.\n")
	}
}

// capList returns at most max entries, so a long estate does not bury the section that follows it.
func capList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := make([]string, 0, max+1)
	out = append(out, items[:max]...)
	return append(out, fmt.Sprintf("and %d more", len(items)-max))
}

// init registers the assess command.
func init() {
	rootCmd.AddCommand(assessCmd)
}
