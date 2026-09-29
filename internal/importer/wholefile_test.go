package importer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// wholeFileFormats is every importer that reads one structured document, with a small export it
// accepts. Each entry is doubled to make a file holding two documents, which is what a shop produces
// by appending one dump to another or by piping through something that adds a line.
var wholeFileFormats = []struct {
	// Name is the format, as the operator names it on the command line.
	Name string
	// From parses a document.
	From func([]byte, time.Time) (*Plan, error)
	// Doc is a small export this format accepts, used both alone and doubled.
	Doc string
	// Join is what sits between the two documents. YAML needs its separator, since two sequences
	// appended without one are a single longer sequence and that file is complete rather than truncated.
	Join string
}{{
	Name: "awx",
	From: FromAWX,
	Doc: `{"projects":[{"name":"web","scm_type":"git","scm_url":"https://example.invalid/w.git"}],` +
		`"job_templates":[{"name":"Deploy","playbook":"site.yml","project":"web"}]}`,
}, {
	Name: "semaphore",
	From: FromSemaphore,
	Doc: `{"repositories":[{"name":"web","git_url":"https://example.invalid/w.git"}],` +
		`"templates":[{"name":"Deploy","playbook":"site.yml","repository":"web"}]}`,
}, {
	Name: "puppet",
	From: FromPuppet,
	Doc:  `[{"certname":"a.prod","catalog_environment":"production","latest_report_status":"changed"}]`,
}, {
	Name: "chef",
	From: FromChef,
	Doc:  `[{"name":"web01.prod","chef_environment":"production","run_list":["role[base]"]}]`,
}, {
	Name: "rundeck",
	From: FromRundeck("prod"),
	Doc:  "- name: Deploy\n  group: web\n  sequence:\n    commands:\n      - exec: /usr/bin/deploy\n",
	Join: "---\n",
}, {
	Name: "jenkins",
	From: FromJenkins("prod"),
	Doc: `<project><description>Deploy</description><builders>` +
		`<hudson.tasks.Shell><command>/usr/bin/deploy</command></hudson.tasks.Shell>` +
		`</builders></project>`,
}}

// TestAnImporterRefusesAFileHoldingMoreThanOneDocument covers an estate that imported in part while
// the report counted the whole of it.
//
// A decoder reads one value and stops. FromAWX moved from json.Unmarshal to a decoder for number
// precision, and json.Unmarshal is what had been refusing trailing data, so two exports concatenated
// imported only the first: 53 objects in the file, "total 3, comes across 3, does not come across 0",
// exit zero, no warning. The same shape covers a dump with a trailing warning line or a proxy's error
// page stapled to the end.
//
// Written over every format rather than over AWX, because the rule is not about AWX. An importer that
// reads part of a file must not report on it as though it read all of it, and the one format that
// stopped doing that is the one nothing was watching.
func TestAnImporterRefusesAFileHoldingMoreThanOneDocument(t *testing.T) {
	t.Parallel()
	for _, format := range wholeFileFormats {
		t.Run(format.Name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

			// The single document has to import, or doubling it proves nothing.
			plan, err := format.From([]byte(format.Doc), now)
			if err != nil {
				t.Fatalf("the single document does not import, so this case tests nothing: %v", err)
			}
			if plan.objects() == 0 {
				t.Fatalf("the single document imported no objects, so this case tests nothing")
			}
			single := plan.objects()

			doubled := format.Doc + "\n" + format.Join + format.Doc + "\n"
			got, err := format.From([]byte(doubled), now)
			if err == nil {
				t.Fatalf("a file holding two documents imported %d object(s) and reported no error. "+
					"The single document holds %d, so %s reads part of a file and counts it as the "+
					"whole estate. An operator sees a total that matches what came across and has no "+
					"way to know the rest of the file was never read.",
					got.objects(), single, format.Name)
			}
			if !strings.Contains(err.Error(), "more than one document") {
				t.Errorf("the refusal does not say the file holds more than one document, so the "+
					"operator is sent to look at the wrong thing: %v", err)
			}
		})
	}
}

// TestTheRefusalNamesWhatWasAppended holds the error to being actionable.
//
// "Unreadable" sends somebody to check a file that parses. Naming where the second document starts
// says what to remove, which is usually obvious once seen and invisible until then.
func TestTheRefusalNamesWhatWasAppended(t *testing.T) {
	t.Parallel()
	first := `{"projects":[{"name":"web","scm_type":"git","scm_url":"https://example.invalid/w.git"}]}`
	second := `{"credentials":[{"name":"vault-prod","credential_type":"Vault","inputs":{}}]}`
	_, err := FromAWX([]byte(first+"\n"+second), time.Unix(0, 0).UTC())
	if err == nil {
		t.Fatal("two documents imported without an error")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("the refusal does not show what was appended, so the operator cannot tell which "+
			"half of the file to remove: %v", err)
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("the refusal spans lines, which breaks a one-line error surface: %q", err)
	}
}

// TestTheUnreadGuardStillSpeaksWhenAFileHasATail covers the half of this that was worse than a silent
// drop.
//
// The unread-field guard scans the raw bytes for anything the importer's structs have no member for.
// It unmarshaled them, which refuses trailing data, and returned nothing on that error. So a file with
// a second document made the guard go quiet: the one warning that exists to catch data nobody looked
// at reported nothing about the file where the most data went unlooked-at, and the summary said no
// fields went unread because it had failed to read any of them.
//
// The assertion that json.Unmarshal refuses these same bytes is the control. It is why the old code
// returned nil, and it proves this case is not passing because the tail is harmless.
func TestTheUnreadGuardStillSpeaksWhenAFileHasATail(t *testing.T) {
	t.Parallel()
	const doc = `{"execution_environments":[{"name":"ee-rhel9"}],` +
		`"projects":[{"name":"web","scm_type":"git","scm_url":"https://example.invalid/w.git"}]}`
	withTail := []byte(doc + "\n" + `{"instance_groups":[{"name":"prod-ig"}]}` + "\n")

	var control any
	if err := json.Unmarshal(withTail, &control); err == nil {
		t.Fatal("json.Unmarshal accepted trailing data, so this case no longer covers what it was " +
			"written for and its result proves nothing")
	}

	var export awxExport
	paths := unreadPaths(withTail, &export)
	if len(paths) == 0 {
		t.Fatal("the unread guard found nothing in a file that carries execution_environments, which " +
			"this importer does not read. A tail turned the guard off, so the export the operator was " +
			"least informed about is the one they were told nothing about.")
	}
	if !strings.Contains(strings.Join(paths, ","), "execution_environments") {
		t.Errorf("the guard ran but did not name execution_environments: %v", paths)
	}
}

// TestTheJenkinsShapeAnExporterActuallyWrites covers the case the first version of this rule missed.
//
// Jenkins writes an XML declaration at the top of every config.xml, so two of them appended put a
// declaration in the middle of the file. Continuing to read with the decoder that had just read the
// first element stops there with a syntax error, and an error is not "nothing follows": the shape
// without declarations was refused while the shape a real export has went through. The case below is
// the one somebody produces with cat, so it is the one that has to hold.
//
// The trailing comment is the other half. It is legal after a root element and the document is whole,
// so refusing it would turn a complete export into a dead end over punctuation.
func TestTheJenkinsShapeAnExporterActuallyWrites(t *testing.T) {
	t.Parallel()
	const job = "<?xml version='1.1' encoding='UTF-8'?>\n" +
		"<project><description>Deploy</description><builders>" +
		"<hudson.tasks.Shell><command>/usr/bin/deploy</command></hudson.tasks.Shell>" +
		"</builders></project>\n"
	now := time.Unix(0, 0).UTC()

	if _, err := FromJenkins("prod")([]byte(job), now); err != nil {
		t.Fatalf("one declared config.xml must import: %v", err)
	}
	if _, err := FromJenkins("prod")([]byte(job+"<!-- exported by the ops team -->\n"), now); err != nil {
		t.Errorf("a comment after the root element is legal and the document is complete, so this is a "+
			"refusal of a file that was fine: %v", err)
	}
	_, err := FromJenkins("prod")([]byte(job+job), now)
	if err == nil {
		t.Fatal("two declared config.xml files imported as one job with no warning, which is the " +
			"shape cat produces and the shape an operator is most likely to hand over")
	}
	if !strings.Contains(err.Error(), "more than one document") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}
