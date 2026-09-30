package importer

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestAnExportFieldNobodyReadsIsReportedRatherThanDropped is the guard on the one import result an
// operator acts on without reading: the clean one.
//
// Every other warning is about something the importer saw and decided about. A field the struct has
// no member for is never seen, so it was dropped with no decision, no warning, and a summary that
// counted zero objects left out and meant it. The fields below are not hypothetical: an AWX shop
// pins collections with an execution environment on every job template, and cutting over to a
// different ansible-core without being told is how a migration breaks in production rather than in
// preview.
func TestAnExportFieldNobodyReadsIsReportedRatherThanDropped(t *testing.T) {
	t.Parallel()
	const export = `{
	  "execution_environments": [{"name": "ee-rhel9"}],
	  "instance_groups": [{"name": "prod-ig"}],
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.invalid/w.git"}],
	  "job_templates": [{
	    "name": "Deploy", "playbook": "site.yml", "project": "web",
	    "execution_environment": "ee-rhel9",
	    "labels": ["tier:web"],
	    "ask_variables_on_launch": true
	  }]
	}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	report := plan.Report()

	// Test 0: Each unread field is named, so an operator can act on it rather than hunt for it.
	joined := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{
		"execution_environments", "instance_groups",
		"job_templates[].execution_environment", "job_templates[].labels",
		"job_templates[].ask_variables_on_launch",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %q, so it was dropped silently:\n%s", want, joined)
		}
	}

	// Test 1: It counts as something that does not come across, not as something worth a look. A
	// dropped execution environment filed under "review" understates what the migration costs.
	if len(report.LeftOut) == 0 {
		t.Errorf("unread fields were not counted as left out: %+v", report)
	}

	// Test 2: A field the struct does read is never reported, or the warning becomes noise nobody
	// finishes reading and the real drops hide inside it.
	for _, consumed := range []string{"job_templates[].limit", "job_templates[].extra_vars",
		"job_templates[].job_tags", "job_templates[].timeout", "job_templates[].forks"} {
		if strings.Contains(joined, consumed) {
			t.Errorf("%q is read by the importer but was reported as unread", consumed)
		}
	}
}

// TestAnExportThisImporterFullyReadsReportsNothingLeftOut is the other half, and the reason the
// first half is trustworthy. A scan that flags something on every document trains a reader to skip
// it, which restores exactly the silence it was written to break.
func TestAnExportThisImporterFullyReadsReportsNothingLeftOut(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.invalid/w.git"}],
	  "inventories": [{"name": "prod", "hosts": [{"name": "web01"}]}],
	  "job_templates": [{
	    "name": "Deploy", "playbook": "site.yml", "project": "web", "inventory": "prod",
	    "limit": "web", "job_tags": "deploy", "verbosity": 2, "forks": 25, "timeout": 3600
	  }]
	}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	if got := plan.Report().LeftOut; len(got) != 0 {
		t.Errorf("an export this importer reads in full reported %d left out:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// TestTheEnvelopeAnExportApiWrapsObjectsInIsNotReportedAsLost keeps the scan usable on a real
// export. `awx export` stamps every object with id, type, url, created, modified, and a
// summary_fields cache of data held elsewhere. None of it is configuration, and naming all of it on
// every object would bury the fields that matter.
func TestTheEnvelopeAnExportApiWrapsObjectsInIsNotReportedAsLost(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [{
	    "id": 7, "type": "project", "url": "/api/v2/projects/7/",
	    "created": "2026-01-01T00:00:00Z", "modified": "2026-01-02T00:00:00Z",
	    "summary_fields": {"organization": {"name": "Default"}},
	    "name": "web", "scm_type": "git", "scm_url": "https://example.invalid/w.git"
	  }]
	}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	if got := plan.Report().LeftOut; len(got) != 0 {
		t.Errorf("export envelope reported as lost configuration:\n%s", strings.Join(got, "\n"))
	}
}

// TestUnreadPathsReportsByShapeRatherThanByIndex keeps a large estate readable. A thousand templates
// carrying the same unread field is one fact about the importer, not a thousand facts about the
// export, and rendering it a thousand times is the same as rendering it zero times.
func TestUnreadPathsReportsByShapeRatherThanByIndex(t *testing.T) {
	t.Parallel()
	var tpl []string
	for i := range 200 {
		tpl = append(tpl, fmt.Sprintf(
			`{"name":"t%d","playbook":"p.yml","execution_environment":"ee"}`, i))
	}
	export := `{"job_templates":[` + strings.Join(tpl, ",") + `]}`

	got := unreadPaths([]byte(export), awxExport{})
	want := []string{"job_templates[].execution_environment"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestTheUnreadSetComesFromTheStructRatherThanAList is what keeps this from rotting.
//
// A hand-kept list of known fields drifts the moment somebody adds one, and it drifts silently in
// the direction of claiming to read more than it does. Taking the set from the struct tags means
// adding a field is what marks it read, so the two cannot disagree. This asserts that coupling
// directly: a field present in the tags is never reported, and the same name outside them always
// is.
func TestTheUnreadSetComesFromTheStructRatherThanAList(t *testing.T) {
	t.Parallel()
	fields := jsonFields(reflect.TypeOf(awxJobTemplate{}))
	if len(fields) < 5 {
		t.Fatalf("job template struct exposes only %d json fields, which cannot be right", len(fields))
	}
	for name := range fields {
		doc := fmt.Sprintf(`{"job_templates":[{"name":"t","playbook":"p.yml",%q:null}]}`, name)
		var probe any
		if json.Unmarshal([]byte(doc), &probe) != nil {
			continue
		}
		for _, p := range unreadPaths([]byte(doc), awxExport{}) {
			if p == "job_templates[]."+name {
				t.Errorf("%q is a struct field yet was reported unread", name)
			}
		}
	}
	// A name the struct does not carry is always reported, in the same position.
	doc := `{"job_templates":[{"name":"t","playbook":"p.yml","no_such_field_anywhere":1}]}`
	found := false
	for _, p := range unreadPaths([]byte(doc), awxExport{}) {
		if p == "job_templates[].no_such_field_anywhere" {
			found = true
		}
	}
	if !found {
		t.Error("a field absent from the struct was not reported as unread")
	}
}

// TestEveryUnreadFieldIsNamed pins the whole list. The report named twelve fields and summarized the
// rest as "and N more", and a Semaphore template's arguments, the field that decided which hosts a
// deploy reached, was among the ones a reader never saw.
func TestEveryUnreadFieldIsNamed(t *testing.T) {
	t.Parallel()
	var fields []string
	for i := range 20 {
		fields = append(fields, fmt.Sprintf(`"unread_%02d": true`, i))
	}
	export := `{"templates": [{"name": "t", "playbook": "site.yml", ` + strings.Join(fields, ", ") + `}]}`
	plan, err := FromSemaphore([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromSemaphore: %v", err)
	}
	joined := strings.Join(plan.Warnings, "\n")
	for i := range 20 {
		if want := fmt.Sprintf("templates[].unread_%02d", i); !strings.Contains(joined, want) {
			t.Errorf("no warning names %q, so it was dropped out of sight:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, " more") {
		t.Errorf("the list was cut short with a count instead of the names:\n%s", joined)
	}
}

// TestOneUnreadFieldIsNamedInTheSingular pins the grammar of the unread warning. One field read as
// "1 field this importer does not read, so they are not imported", a sentence that stops agreeing
// with itself on exactly the export an operator is most likely to read closely.
func TestOneUnreadFieldIsNamedInTheSingular(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Export     string
		WantResult string
	}{{ // Test 0: One field is it.
		Export: `{"templates": [{"name": "t", "playbook": "site.yml", "unread_a": true}]}`,
		WantResult: "this export holds 1 field this importer does not read, so it is not " +
			"imported: templates[].unread_a",
	}, { // Test 1: Two fields are they.
		Export: `{"templates": [{"name": "t", "playbook": "site.yml", "unread_a": true,
			"unread_b": true}]}`,
		WantResult: "this export holds 2 fields this importer does not read, so they are not " +
			"imported: templates[].unread_a, templates[].unread_b",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromSemaphore([]byte(test.Export), time.Now())
			if err != nil {
				t.Fatalf("FromSemaphore: %v", err)
			}
			var got string
			for _, w := range plan.Warnings {
				if strings.HasPrefix(w, "this export holds ") {
					got = w
				}
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("unread warning mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
