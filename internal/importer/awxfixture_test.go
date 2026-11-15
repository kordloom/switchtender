package importer

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// vendorFixture reads one of the vendor-shaped exports in testdata. Each is built key for key from
// the export code its README names, at the tag it names.
func vendorFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return raw
}

// withoutTopLevelKey returns an export with one top-level key removed, which is what awxkit writes
// when it is asked for every resource but that one.
func withoutTopLevelKey(t *testing.T, raw []byte, key string) []byte {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	delete(doc, key)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return out
}

// TestVendorExportFixturesImportAsTheQuorumSays reads the export each client writes, built from its
// published export code, and pins what the import makes of it.
//
// awxkit 24.6.1 writes the approval node, so the release workflow keeps its gate. awxkit 17.1.0,
// of the Tower 3.8 era, drops the approval node that opens the workflow, so the deploy and its
// deny path arrive as two steps that start together, and the only notice is the old-client
// warning. ascender-kit 25.6.3 writes a two-person approval and a conditional connector, which
// refuse their workflows by name, and an approval that approves itself on a timeout and carries a
// context template, which import with a warning each, beside a node's retries.
//
//nolint:funlen // Test function.
func TestVendorExportFixturesImportAsTheQuorumSays(t *testing.T) {
	t.Parallel()
	release := []run.PipelineStep{
		{Name: "approve", Type: run.StepApproval, ApprovalTimeout: 3600,
			Description: "Approve the release: Check the canary before production"},
		{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"approve"}},
		{Name: "page", Playbook: "page.yml", IfDenied: []string{"approve"}},
	}
	tests := []struct {
		// Fixture names the file in testdata.
		Fixture string
		// DropKey names a top-level key to remove first, to stand for a partial export.
		DropKey string
		// WantKeys are the top-level keys the client's EXPORTABLE_RESOURCES makes it write.
		WantKeys []string
		// WantOldClient reports whether the old-client warning must lead the preview.
		WantOldClient bool
		// WantSteps are the steps of each workflow that must import, by name.
		WantSteps map[string][]run.PipelineStep
		// WantRefused are fragments of the refusal each refused workflow must carry, by name.
		WantRefused map[string]string
		// WantReview are fragments of warnings that must be counted as worth reviewing.
		WantReview []string
		// WantSchedules is how many schedules the import creates.
		WantSchedules int
	}{{ // Test 0: The current awx client writes the approval, so the gate comes across.
		Fixture:       "awxkit-24.6.1-export.json",
		WantKeys:      awxkitResources("21.3.0"),
		WantSteps:     map[string][]run.PipelineStep{"Release": release},
		WantSchedules: 1,
	}, { // Test 1: A partial export from that client lacks the schedules list and still holds
		// an approval node, which proves the client is not an old one.
		Fixture:       "awxkit-24.6.1-export.json",
		DropKey:       "schedules",
		WantSteps:     map[string][]run.PipelineStep{"Release": release},
		WantSchedules: 1,
	}, { // Test 2: The Tower 3.8 era client drops the opening approval without a trace.
		Fixture:       "awxkit-17.1.0-export.json",
		WantKeys:      awxkitResources("17.1.0"),
		WantOldClient: true,
		WantSteps: map[string][]run.PipelineStep{"Release": {
			{Name: "deploy", Playbook: "deploy.yml"},
			{Name: "page", Playbook: "page.yml"},
		}},
		WantSchedules: 1,
	}, { // Test 3: Ascender's two-person approval and conditional connector refuse by name.
		Fixture:  "ascender-kit-25.6.3-export.json",
		WantKeys: awxkitResources("21.3.0"),
		WantSteps: map[string][]run.PipelineStep{"Nightly": {
			{Name: "approve", Type: run.StepApproval, Description: "Approve the nightly",
				ApprovalTimeout: 3600},
			{Name: "deploy", Playbook: "deploy.yml", Retries: 2, DependsOn: []string{"approve"}},
			{Name: "page", Playbook: "page.yml", IfDenied: []string{"approve"}},
		}},
		WantRefused: map[string]string{
			"Release": `approval node approve, "Approve the release", waits for 2 distinct people`,
			"Canary":  `node check leads to "deploy" through a conditional connector`,
		},
		WantReview: []string{`workflow "Nightly" approval node approve, "Approve the nightly", ` +
			"approves itself in Ascender"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw := vendorFixture(t, test.Fixture)
			if test.WantKeys != nil {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("Unmarshal() error = %v", err)
				}
				got := slices.Sorted(maps.Keys(doc))
				want := slices.Sorted(slices.Values(test.WantKeys))
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("top-level keys mismatch (-want +got):\n%s", diff)
				}
			}
			if test.DropKey != "" {
				raw = withoutTopLevelKey(t, raw, test.DropKey)
			}
			plan, err := FromAWX(raw, importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			joined := strings.Join(plan.Warnings, "\n")

			r := plan.Report()
			gotOld := len(r.NeedsReview) > 0 && strings.Contains(r.NeedsReview[0], oldClientWarning)
			if gotOld != test.WantOldClient {
				t.Errorf("old-client warning leads = %v, want %v.\nwarnings:\n%s", gotOld,
					test.WantOldClient, joined)
			}
			if w, ok := warningContaining(t, plan.Warnings, oldClientWarning); ok && !test.WantOldClient {
				t.Errorf("a file a current client wrote carries the old-client warning: %s", w)
			}

			imported := map[string][]run.PipelineStep{}
			for _, tpl := range plan.Templates {
				if len(tpl.Steps) > 0 {
					imported[tpl.Name] = tpl.Steps
				}
			}
			if diff := cmp.Diff(test.WantSteps, imported, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("imported workflows mismatch (-want +got):\n%s\nwarnings:\n%s", diff, joined)
			}
			for name, fragment := range test.WantRefused {
				if _, ok := warningContaining(t, r.LeftOut, fmt.Sprintf("workflow %q was not "+
					"imported", name), fragment); !ok {
					t.Errorf("workflow %q is not refused with %q.\nwarnings:\n%s", name, fragment,
						joined)
				}
			}
			for _, fragment := range test.WantReview {
				if _, ok := warningContaining(t, r.NeedsReview, fragment); !ok {
					t.Errorf("nothing to review says %q.\nwarnings:\n%s", fragment, joined)
				}
			}
			if len(plan.Schedules) != test.WantSchedules {
				t.Errorf("schedules = %d, want %d.\nwarnings:\n%s", len(plan.Schedules),
					test.WantSchedules, joined)
			}
			// Every field the Ascender gate decides on is read, so none is named as unread.
			if w, ok := warningContaining(t, plan.Warnings, "fields this importer does not read"); ok {
				for _, field := range []string{"condition_nodes", "max_retries", "required_approvals",
					"on_timeout", "context_template", "create_approval_template"} {
					if strings.Contains(w, field) {
						t.Errorf("the unread-fields warning names %s, which the import reads: %s",
							field, w)
					}
				}
			}
		})
	}
}

// awxkitResources returns the top-level keys awx export writes on a full export, from
// EXPORTABLE_RESOURCES in awxkit/awxkit/api/pages/api.py. since names the awxkit tag whose list is
// returned: 17.1.0 writes eleven keys, 21.2.0 already adds execution_environments and
// applications, and 21.3.0 adds schedules. ascender-kit 25.6.3 writes the 21.3.0 list.
func awxkitResources(since string) []string {
	keys := []string{"users", "organizations", "teams", "credential_types", "credentials",
		"notification_templates", "projects", "inventory", "inventory_sources", "job_templates",
		"workflow_job_templates"}
	if since == "17.1.0" {
		return keys
	}
	return append(keys, "execution_environments", "applications", "schedules")
}
