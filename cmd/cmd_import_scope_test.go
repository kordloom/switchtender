package cmd

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/template"
)

// TestTemplateScopeShowsWhatNarrowsARun pins the plan line for an imported template. A limit or check
// mode that came across was invisible in the preview, so a reader could not tell a carried limit from
// a dropped one, which is the difference between a deploy to one tier and a deploy to all of them.
func TestTemplateScopeShowsWhatNarrowsARun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult string
		In         template.Template
	}{{ // Test 0: A template with nothing narrowing it prints its name alone.
		WantResult: "",
	}, { // Test 1: A limit.
		In: template.Template{Limit: "web"}, WantResult: " (limit web)",
	}, { // Test 2: Everything a run can be narrowed or changed by, in one line.
		In: template.Template{
			Limit: "web", Tags: []string{"deploy", "config"}, SkipTags: []string{"slow"},
			DryRun: true, DiffMode: true, Forks: 5, Verbosity: 2,
			ExtraVars: map[string]any{"env": "prod"},
		},
		WantResult: " (limit web, tags deploy,config, skip tags slow, check mode, diff, forks 5, " +
			"verbosity 2, 1 extra var)",
	}, { // Test 3: The plural reads as one.
		In:         template.Template{ExtraVars: map[string]any{"a": 1, "b": 2}},
		WantResult: " (2 extra vars)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, templateScope(&test.In)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
