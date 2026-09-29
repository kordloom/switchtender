package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/importer"
)

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
		mapper, ok := importer.Readers[format]
		if !ok {
			return fmt.Errorf("unknown format %q: expected one of %s",
				format, strings.Join(importer.Formats(), ", "))
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("read export: %w", err)
		}
		plan, err := mapper(data, time.Now())
		if err != nil {
			return err
		}
		importer.Render(cmd.OutOrStdout(), format, args[1], plan.Assess())
		return nil
	},
}

// init registers the assess command.
func init() {
	rootCmd.AddCommand(assessCmd)
}
