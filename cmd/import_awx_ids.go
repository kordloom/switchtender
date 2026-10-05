package cmd

import (
	"fmt"
	"os"

	"github.com/kordloom/switchtender/internal/importer"
)

// importAWXTemplateIDs holds the import awx --awx-template-ids flag: files holding AWX's job
// template list.
var importAWXTemplateIDs []string

// init registers the flag that binds imported templates to their AWX job template ids.
func init() {
	importAWXCmd.Flags().StringArrayVar(&importAWXTemplateIDs, "awx-template-ids", nil,
		"AWX's job template list, GET /api/v2/job_templates/ saved as JSON, so a template that "+
			"accepted callbacks also answers at its AWX callback address when the export carries no "+
			"ids. Repeatable, one file per page.")
}

// awxMapper returns the mapper import awx runs: FromAWX, or FromAWX with the AWX ids the
// --awx-template-ids files give.
func awxMapper() (mapFunc, error) {
	if len(importAWXTemplateIDs) == 0 {
		return importer.FromAWX, nil
	}
	lists := make([][]byte, 0, len(importAWXTemplateIDs))
	for _, path := range importAWXTemplateIDs {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the awx job template list: %w", err)
		}
		lists = append(lists, data)
	}
	return importer.FromAWXWithTemplateIDs(lists...)
}
