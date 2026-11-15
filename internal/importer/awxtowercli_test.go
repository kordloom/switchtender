package importer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// towerCLIJSON is a file in the shape tower-cli receive writes by default: a list of assets, each
// tagged with asset_type, its POST fields beside the tag and its relations under asset_relation.
// The shape is from tower_cli/cli/transfer/receive.py and common.py at ansible/tower-cli tag
// v3.3.9, read 2026-11-12.
const towerCLIJSON = `[
  {
    "asset_type": "project",
    "name": "Infra",
    "description": "",
    "scm_type": "git",
    "scm_url": "https://git.example.com/infra.git",
    "scm_branch": "main",
    "organization": "Default",
    "asset_relation": {"roles": [], "schedules": [], "notification_templates_error": []}
  },
  {
    "asset_type": "job_template",
    "name": "Deploy Web",
    "playbook": "site.yml",
    "project": "Infra",
    "inventory": "Production",
    "asset_relation": {"credentials": [], "labels": [], "schedules": [], "survey_spec": {}}
  }
]`

// towerCLIYAML is the same file as tower-cli receive writes it when asked for YAML.
const towerCLIYAML = `- asset_type: project
  name: Infra
  scm_type: git
  scm_url: https://git.example.com/infra.git
  organization: Default
  asset_relation:
    roles: []
- asset_type: job_template
  name: Deploy Web
  playbook: site.yml
  project: Infra
`

// TestAWXNamesATowerCLIFile pins that a file from tower-cli receive is refused with its cause and
// the command to run instead, rather than as a parse error that reads as a broken file.
func TestAWXNamesATowerCLIFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the document handed to the importer.
		In string
		// WantTowerCLI reports whether the refusal must name tower-cli.
		WantTowerCLI bool
	}{{ // Test 0: The JSON tower-cli receive writes by default.
		In: towerCLIJSON, WantTowerCLI: true,
	}, { // Test 1: The YAML it writes when asked.
		In: towerCLIYAML, WantTowerCLI: true,
	}, { // Test 2: A YAML document marker ahead of the list.
		In: "---\n" + towerCLIYAML, WantTowerCLI: true,
	}, { // Test 3: One tagged entry is enough, whatever else the list holds.
		In: `[1, {"asset_type": "team", "name": "ops"}]`, WantTowerCLI: true,
	}, { // Test 4: A list with no tag is some other file, so the parse error stands.
		In: `[{"name": "x"}]`,
	}, { // Test 5: A YAML list with no tag is some other file too.
		In: "- name: x\n",
	}, { // Test 6: A broken awx export is not called a tower-cli file.
		In: `{"projects": [`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(test.In), importNow)
			if err == nil {
				t.Fatalf("FromAWX() error = nil, want a refusal; plan = %+v", plan)
			}
			named := strings.Contains(err.Error(), "tower-cli receive")
			if named != test.WantTowerCLI {
				t.Fatalf("names tower-cli = %v, want %v: %v", named, test.WantTowerCLI, err)
			}
			if !test.WantTowerCLI {
				return
			}
			// The server answers this class of refusal with the importer's own sentence, so the
			// person who sent the file reads the cause and the remedy rather than a format error.
			if !errors.Is(err, ErrNothingRecognized) {
				t.Errorf("error = %v, want one matching ErrNothingRecognized", err)
			}
			if !strings.Contains(err.Error(), "awx export > export.json") {
				t.Errorf("the refusal does not say what to run instead: %v", err)
			}
		})
	}
}
