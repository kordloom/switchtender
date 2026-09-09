package importer

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// Committed archives taken from a real Rundeck 5.8.0, one per node source kind, since what the
// project pointed at is the whole of what an archive can say about the hosts its jobs ran on.
const (
	// fileSourceArchive is a project whose nodes came from a file on the Rundeck server. It also
	// carries an SCM export configuration and two jobs, one of them scheduled.
	fileSourceArchive = "rundeck-archive-file-nodesource.zip"
	// urlSourceArchive is a project whose nodes came from a remote endpoint, with no SCM
	// configuration at all.
	urlSourceArchive = "rundeck-archive-url-nodesource.zip"
)

// readArchiveFixture returns one committed project archive.
func readArchiveFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// planFromArchive imports one committed project archive and fails the test if it will not import.
func planFromArchive(t *testing.T, name, inventory string) *Plan {
	t.Helper()
	plan, err := FromRundeck(inventory)(readArchiveFixture(t, name), testNow)
	if err != nil {
		t.Fatalf("FromRundeck(%s) error = %v", name, err)
	}
	return plan
}

// templateNames lists a plan's template names in plan order.
func templateNames(plan *Plan) []string {
	out := make([]string, 0, len(plan.Templates))
	for _, tmpl := range plan.Templates {
		out = append(out, tmpl.Name)
	}
	return out
}

// scheduleSpecs maps a plan's schedule names to the cron expressions they will fire on.
func scheduleSpecs(plan *Plan) map[string]string {
	out := map[string]string{}
	for _, sc := range plan.Schedules {
		out[sc.Name] = sc.Cron
	}
	return out
}

// wantWarning fails the test unless some warning carries the given text.
func wantWarning(t *testing.T, plan *Plan, text string) {
	t.Helper()
	for _, w := range plan.Warnings {
		if strings.Contains(w, text) {
			return
		}
	}
	t.Errorf("no warning mentions %q; warnings were:\n  %s", text,
		strings.Join(plan.Warnings, "\n  "))
}

// TestFromRundeckArchive covers both committed project archives end to end.
//
// The two are the same artifact carrying different node sources, and the node source is the whole
// question: a project archive names where the hosts came from and carries not one of them, so the
// case that matters is that templates arrive, an inventory never does, and the report says what the
// project pointed at so the operator can attach the right one themselves.
func TestFromRundeckArchive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Archive       string
		Inventory     string
		WantTemplates []string
		WantSchedules map[string]string
		WantProjects  []string
		WantWarnings  []string
	}{{ // Test 0: A project whose nodes came from a file on the Rundeck server.
		Archive: fileSourceArchive, Inventory: "prod-hosts",
		WantTemplates: []string{"batch/nightly-settlement-run", "maintenance/rotate-batch-logs"},
		WantSchedules: map[string]string{"batch/nightly-settlement-run": "30 2 * * *"},
		WantWarnings: []string{
			"a file on the Rundeck server at %PROJECT_BASEDIR%/etc/resources.xml",
			"No inventory was imported from it",
			"file:///home/rundeck/gitrepos/payments-batch-jobs.git",
			"so no project was created",
		},
	}, { // Test 1: A project whose nodes came from a remote endpoint, with no SCM configured.
		Archive: urlSourceArchive, Inventory: "cmdb-hosts",
		WantTemplates: []string{"ops/patch-sweep"},
		WantWarnings: []string{
			"the endpoint https://cmdb.internal.example.com/api/v1/rundeck/nodes.yaml",
			"No inventory was imported from it",
			"the archive carries no SCM configuration, so no project was created",
		},
	}, { // Test 2: No inventory named, which every template must be told about.
		Archive: urlSourceArchive, Inventory: "",
		WantTemplates: []string{"ops/patch-sweep"},
		WantWarnings:  []string{"no inventory was named"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := planFromArchive(t, test.Archive, test.Inventory)
			if diff := cmp.Diff(test.WantTemplates, templateNames(plan),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("templates mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSchedules, scheduleSpecs(plan),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("schedules mismatch (-want +got):\n%s", diff)
			}
			// An archive carries the node source configuration and no node definitions, so an
			// inventory or a dynamic source appearing here would have been invented.
			if len(plan.Inventories) != 0 || len(plan.Sources) != 0 {
				t.Errorf("inventories = %d and sources = %d, want none from an archive",
					len(plan.Inventories), len(plan.Sources))
			}
			if len(plan.Credentials) != 0 {
				t.Errorf("credentials = %d, want none from an archive", len(plan.Credentials))
			}
			var projects []string
			for _, p := range plan.Projects {
				projects = append(projects, p.Name)
			}
			if diff := cmp.Diff(test.WantProjects, projects, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("projects mismatch (-want +got):\n%s", diff)
			}
			for _, want := range test.WantWarnings {
				wantWarning(t, plan, want)
			}
			// Every template lands against the inventory the operator named, the same as the job
			// export path, since neither artifact names a host of its own.
			for _, tmpl := range plan.Templates {
				if tmpl.Inventory != test.Inventory {
					t.Errorf("template %q inventory = %q, want %q",
						tmpl.Name, tmpl.Inventory, test.Inventory)
				}
			}
		})
	}
}

// TestRundeckArchiveMatchesJobExport proves the two artifacts produce the same objects for the same
// job.
//
// The archive writes jobs as XML and a job export writes them as YAML, in different shapes: options
// carry their settings as attributes, dispatch sits beside the node filter rather than inside it,
// and the schedule folds the day of month onto the month element. If the archive were mapped by its
// own code path the two would drift, and a team that exported one artifact would get a different
// template from a team that exported the other. So the XML is converted into the export's shape and
// run through the one mapping, and this pins that they land on the same template and the same
// schedule.
func TestRundeckArchiveMatchesJobExport(t *testing.T) {
	t.Parallel()
	// The YAML spelling of the archive's nightly-settlement-run job, field for field.
	const export = `
- name: nightly-settlement-run
  group: batch
  project: payments-batch
  description: Run the nightly settlement close on the payment workers.
  executionEnabled: true
  scheduleEnabled: true
  nodefilters:
    filter: 'tags: payments+worker'
    dispatch:
      threadcount: 2
      keepgoing: false
  options:
    - name: settlement_date
      description: Business date to settle, YYYY-MM-DD.
      value: yesterday
      required: true
    - name: dry_run
      description: Report only, do not post entries.
      value: 'true'
      values: ['true', 'false']
      enforced: true
  sequence:
    keepgoing: false
    commands:
      - description: Verify the batch mount is present
        exec: test -d /srv/payments/batch
      - description: Run the settlement close
        scriptinterpreter: /bin/bash
        script: |
          set -euo pipefail
          /opt/payments/bin/settle --date "$RD_OPTION_SETTLEMENT_DATE" --dry-run="$RD_OPTION_DRY_RUN"
  schedule:
    time:
      hour: '2'
      minute: '30'
      seconds: '0'
    month: '*'
    dayofmonth:
      day: '*'
    year: '*'
`
	const job = "batch/nightly-settlement-run"
	fromExport, err := FromRundeck("prod-hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck(job export) error = %v", err)
	}
	fromArchive := planFromArchive(t, fileSourceArchive, "prod-hosts")

	// The generated ids differ by construction and are wired within each plan, so they are compared
	// by whether they still point at the same object rather than by value.
	ignoreIDs := cmpopts.IgnoreFields(template.Template{}, "ID")
	if diff := cmp.Diff(findTemplate(t, fromExport, job), findTemplate(t, fromArchive, job),
		cmpopts.EquateEmpty(), ignoreIDs); diff != "" {
		t.Errorf("the same job imported differently from the two artifacts (-export +archive):\n%s",
			diff)
	}
	exportSchedule, archiveSchedule := findSchedule(t, fromExport, job), findSchedule(t, fromArchive, job)
	if diff := cmp.Diff(exportSchedule, archiveSchedule, cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(schedule.Schedule{}, "ID", "TemplateID")); diff != "" {
		t.Errorf("the same schedule imported differently from the two artifacts "+
			"(-export +archive):\n%s", diff)
	}
	if archiveSchedule.TemplateID != findTemplate(t, fromArchive, job).ID {
		t.Error("the archive's schedule does not point at the template the archive created")
	}
}

// findTemplate returns the named template from a plan, or fails the test.
func findTemplate(t *testing.T, plan *Plan, name string) *template.Template {
	t.Helper()
	for _, tmpl := range plan.Templates {
		if tmpl.Name == name {
			return tmpl
		}
	}
	t.Fatalf("no template named %q; got %v (warnings %v)", name, templateNames(plan), plan.Warnings)
	return nil
}

// findSchedule returns the named schedule from a plan, or fails the test.
func findSchedule(t *testing.T, plan *Plan, name string) *schedule.Schedule {
	t.Helper()
	for _, sc := range plan.Schedules {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no schedule named %q (warnings %v)", name, plan.Warnings)
	return nil
}

// rundeckArchiveEntries returns the members of a small but complete project archive, which a test
// adds to or overrides before building the zip.
func rundeckArchiveEntries() map[string]string {
	return map[string]string{
		"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n" +
			"Rundeck-Application-Version: 5.8.0-20241205\n" +
			"Rundeck-Archive-Format-Version: 1.0\n" +
			"Rundeck-Archive-Project-Name: payments-batch\n\n",
		"rundeck-payments-batch/files/etc/project.properties": "#Exported configuration\n" +
			"project.name=payments-batch\n" +
			"resources.source.1.config.file=%PROJECT_BASEDIR%/etc/resources.xml\n" +
			"resources.source.1.type=file\n",
		"rundeck-payments-batch/jobs/job-1.xml": "<joblist><job><name>close-books</name>" +
			"<group>batch</group><sequence><command><exec>echo hi</exec></command>" +
			"</sequence></job></joblist>",
	}
}

// TestRundeckArchiveProject covers when an archive's SCM configuration becomes a project and when it
// does not.
//
// A Rundeck project is a job store rather than a git repository, so the only thing in an archive
// that can become a project is the SCM plugin's own repository. A file URL is refused on purpose:
// it names a directory on the Rundeck server, which is a different machine, so a project built from
// it either fails on its first sync or finds an unrelated directory of that name on this host and
// clones that instead. Naming a machine resource this cannot see is the same mistake as inventing an
// inventory.
func TestRundeckArchiveProject(t *testing.T) {
	t.Parallel()
	const scmPath = "rundeck-payments-batch/files/etc/scm-export.properties"
	tests := []struct {
		Name        string
		SCM         string
		WantProject string
		WantRepo    string
		WantBranch  string
		WantWarning string
	}{{ // Test 0: No SCM configuration at all, which is every project that never turned it on.
		Name: "no scm", SCM: "",
		WantWarning: "the archive carries no SCM configuration, so no project was created",
	}, { // Test 1: A real remote, which is a repository this can clone.
		Name: "https remote",
		SCM: "scm.export.config.url=https\\://git.example.com/ops/payments-batch-jobs.git\n" +
			"scm.export.config.branch=main\nscm.export.type=git-export\n",
		WantProject: "payments-batch",
		WantRepo:    "https://git.example.com/ops/payments-batch-jobs.git",
		WantBranch:  "main",
		WantWarning: "was created from the archive's SCM export configuration",
	}, { // Test 2: A file URL, which names a path on a machine this cannot reach.
		Name: "file url",
		SCM: "scm.export.config.url=file\\:///home/rundeck/gitrepos/payments-batch-jobs.git\n" +
			"scm.export.config.branch=main\n",
		WantWarning: "which is a path on the Rundeck server rather than a repository this can reach",
	}, { // Test 3: An SCM configuration with no repository named at all.
		Name: "no url", SCM: "scm.export.config.branch=main\nscm.export.enabled=true\n",
		WantWarning: "names no repository, so no project was created from it",
	}, { // Test 4: A scheme the API itself refuses, which an archive must not smuggle past it.
		Name:        "refused scheme",
		SCM:         "scm.export.config.url=git\\://git.example.com/jobs.git\n",
		WantWarning: "which was not imported as a project",
	}, { // Test 5: A URL still carrying Rundeck's project directory token.
		Name:        "unexpanded token",
		SCM:         "scm.export.config.url=file\\://%PROJECT_BASEDIR%/jobs.git\n",
		WantWarning: "which is a directory on the Rundeck server rather than a repository",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := rundeckArchiveEntries()
			if test.SCM != "" {
				entries[scmPath] = "#Exported configuration\n" + test.SCM
			}
			plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
			if err != nil {
				t.Fatalf("FromRundeck() error = %v", err)
			}
			wantWarning(t, plan, test.WantWarning)
			if test.WantProject == "" {
				if len(plan.Projects) != 0 {
					t.Fatalf("projects = %d, want none", len(plan.Projects))
				}
				return
			}
			if len(plan.Projects) != 1 {
				t.Fatalf("projects = %d, want 1 (warnings %v)", len(plan.Projects), plan.Warnings)
			}
			got := plan.Projects[0]
			if got.Name != test.WantProject || got.RepoURL != test.WantRepo ||
				got.Branch != test.WantBranch {
				t.Errorf("project = %q %q @ %q, want %q %q @ %q", got.Name, got.RepoURL, got.Branch,
					test.WantProject, test.WantRepo, test.WantBranch)
			}
		})
	}
}

// TestRundeckArchiveReportsNodeSources proves every node source kind is named in the report and none
// of them produces an inventory.
//
// The archive carries no node definitions at any path, only the configuration of where Rundeck
// fetched them from. Reporting that configuration is the whole of what can honestly be said, and it
// is what lets an operator attach the right inventory. Building one from a path on another server,
// from an endpoint this has not called, or from the node names in the execution history would each
// be a guess at the list of machines a run reaches.
func TestRundeckArchiveReportsNodeSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Properties  string
		WantWarning string
	}{{ // Test 0: A file on the Rundeck server, whose path carries the project directory token.
		Name: "file source",
		Properties: "resources.source.1.type=file\n" +
			"resources.source.1.config.file=%PROJECT_BASEDIR%/etc/resources.xml\n",
		WantWarning: "a file on the Rundeck server at %PROJECT_BASEDIR%/etc/resources.xml, where " +
			"%PROJECT_BASEDIR% is that project's own directory on that server",
	}, { // Test 1: A remote endpoint, whose URL the properties file escapes its colon in.
		Name: "url source",
		Properties: "resources.source.1.type=url\n" +
			"resources.source.1.config.url=https\\://cmdb.example.com/nodes.yaml\n",
		WantWarning: "the endpoint https://cmdb.example.com/nodes.yaml",
	}, { // Test 2: A directory of node files.
		Name: "directory source",
		Properties: "resources.source.1.type=directory\n" +
			"resources.source.1.config.directory=/var/rundeck/nodes.d\n",
		WantWarning: "a directory on the Rundeck server at /var/rundeck/nodes.d",
	}, { // Test 3: A plugin source this has no reading for, which is still named by its type.
		Name:        "plugin source",
		Properties:  "resources.source.1.type=aws-ec2\nresources.source.1.config.region=us-east-1\n",
		WantWarning: `a "aws-ec2" source`,
	}, { // Test 4: Several sources, each of which the operator has to replace.
		Name: "two sources",
		Properties: "resources.source.1.type=file\nresources.source.1.config.file=/etc/nodes.xml\n" +
			"resources.source.2.type=url\n" +
			"resources.source.2.config.url=https\\://cmdb.example.com/nodes.yaml\n",
		WantWarning: "this project's nodes came from source 2, the endpoint",
	}, { // Test 5: A project that named no source at all.
		Name: "no source", Properties: "project.name=payments-batch\n",
		WantWarning: "the project configuration names no node source",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := rundeckArchiveEntries()
			entries["rundeck-payments-batch/files/etc/project.properties"] =
				"project.name=payments-batch\n" + test.Properties
			plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
			if err != nil {
				t.Fatalf("FromRundeck() error = %v", err)
			}
			wantWarning(t, plan, test.WantWarning)
			wantWarning(t, plan, "No inventory was imported")
			if len(plan.Inventories) != 0 || len(plan.Sources) != 0 {
				t.Errorf("inventories = %d and sources = %d, want none",
					len(plan.Inventories), len(plan.Sources))
			}
		})
	}
}

// TestRundeckArchiveRefusesHostileMembers proves the archive reader refuses a member no export
// writes.
//
// Nothing is written to disk here, so none of these can overwrite a file today. That is a property
// of the current reader rather than of the input, and an archive arrives over an upload endpoint
// anyone allowed to import can reach. Each of these shapes means the zip was built by hand to escape
// wherever it lands, so the whole archive is refused rather than quietly filtered down to the
// members that looked reasonable.
func TestRundeckArchiveRefusesHostileMembers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Member      string
		WantMessage string
	}{{ // Test 0: A path climbing out of wherever the archive is unpacked.
		Name: "traversal", Member: "rundeck-x/jobs/../../../etc/passwd",
		WantMessage: "climbs out of the archive",
	}, { // Test 1: A traversal in a member this reader would not have read anyway.
		Name: "traversal in an ignored member", Member: "../../etc/passwd",
		WantMessage: "climbs out of the archive",
	}, { // Test 2: An absolute path, which lands where it says rather than where it was put.
		Name: "absolute", Member: "/etc/cron.d/rundeck",
		WantMessage: "has an absolute path",
	}, { // Test 3: A Windows drive letter, the absolute path of the other operating system.
		Name: "drive letter", Member: `C:\windows\system32\drivers\etc\hosts`,
		WantMessage: "names a drive",
	}, { // Test 4: A backslash traversal, which the reader normalizes before checking.
		Name: "backslash traversal", Member: `rundeck-x\jobs\..\..\..\etc\passwd`,
		WantMessage: "climbs out of the archive",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := rundeckArchiveEntries()
			entries[test.Member] = "owned"
			_, err := FromRundeck("prod")(buildZip(t, entries), testNow)
			if err == nil {
				t.Fatalf("FromRundeck() error = nil, want the member %q refused", test.Member)
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("error = %v, want it to say the member %s", err, test.WantMessage)
			}
		})
	}
}

// TestRundeckArchiveRefusesSymlinkMember covers the member kind a name check cannot see. A symlink
// entry named innocently still points wherever its body says, so it is refused by its mode rather
// than by its name.
func TestRundeckArchiveRefusesSymlinkMember(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range rundeckArchiveEntries() {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	header := &zip.FileHeader{Name: "rundeck-payments-batch/files/etc/project.properties.link"}
	header.SetMode(0o777 | os.ModeSymlink)
	link, err := w.CreateHeader(header)
	if err != nil {
		t.Fatalf("create symlink entry: %v", err)
	}
	if _, err := link.Write([]byte("/etc/passwd")); err != nil {
		t.Fatalf("write symlink entry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	_, err = FromRundeck("prod")(buf.Bytes(), testNow)
	if err == nil {
		t.Fatal("FromRundeck() error = nil, want the symlink member refused")
	}
	if !strings.Contains(err.Error(), "is a symbolic link") {
		t.Errorf("error = %v, want it to say the member is a symbolic link", err)
	}
}

// TestRundeckArchiveCeilings covers the three bounds on what one archive may cost to read: how many
// members are examined, how large one member may be, and how much is read in total. Each is
// deletable without the others noticing, and an upload endpoint is where a zip bomb arrives, so each
// is pinned on its own.
func TestRundeckArchiveCeilings(t *testing.T) {
	t.Parallel()
	// The archives below hold one member more than each ceiling allows, so they track whatever the
	// ceilings are set to. That alone would still pass if a ceiling were loosened to a number no
	// upload could reach, which is the same as having none, so the ranges are pinned too.
	if maxRundeckZipEntries < 1000 || maxRundeckZipEntries > 100000 {
		t.Fatalf("maxRundeckZipEntries = %d, want a ceiling a hostile upload can reach",
			maxRundeckZipEntries)
	}
	if maxRundeckEntrySize > maxRundeckTotalSize {
		t.Fatalf("maxRundeckEntrySize = %d exceeds maxRundeckTotalSize = %d, so the total ceiling "+
			"can never refuse anything the per-member one allowed",
			maxRundeckEntrySize, maxRundeckTotalSize)
	}

	t.Run("test 0 too many members", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for i := range maxRundeckZipEntries + 1 {
			if _, err := w.Create(fmt.Sprintf("rundeck-x/executions/output-%d.rdlog", i)); err != nil {
				t.Fatalf("create zip entry %d: %v", i, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close zip: %v", err)
		}
		_, err := FromRundeck("prod")(buf.Bytes(), testNow)
		if err == nil || !strings.Contains(err.Error(), "more than the") {
			t.Errorf("error = %v, want the member-count ceiling to refuse the archive", err)
		}
	})

	t.Run("test 1 one member too large", func(t *testing.T) {
		t.Parallel()
		entries := rundeckArchiveEntries()
		entries["rundeck-payments-batch/jobs/job-big.xml"] = strings.Repeat("x", maxRundeckEntrySize+1)
		_, err := FromRundeck("prod")(buildZip(t, entries), testNow)
		if err == nil || !strings.Contains(err.Error(), "larger than a definition should be") {
			t.Errorf("error = %v, want the per-member ceiling to refuse the archive", err)
		}
	})

	t.Run("test 2 too much in total", func(t *testing.T) {
		t.Parallel()
		entries := rundeckArchiveEntries()
		body := strings.Repeat("x", maxRundeckEntrySize)
		for i := range maxRundeckTotalSize/maxRundeckEntrySize + 1 {
			entries[fmt.Sprintf("rundeck-payments-batch/jobs/job-%02d.xml", i)] = body
		}
		_, err := FromRundeck("prod")(buildZip(t, entries), testNow)
		if err == nil || !strings.Contains(err.Error(), "exceed") {
			t.Errorf("error = %v, want the total ceiling to refuse the archive", err)
		}
	})
}

// TestRundeckArchiveRefusesAnotherToolsZip proves a zip that is not a Rundeck project archive is
// refused by name rather than reported as a Rundeck project holding nothing. Handing the Jenkins
// archive to the Rundeck importer is the mistake the detection invites, since neither needs a flag.
func TestRundeckArchiveRefusesAnotherToolsZip(t *testing.T) {
	t.Parallel()
	zipped := buildZip(t, map[string]string{
		"jobs/deploy/config.xml": "<project><builders/></project>",
	})
	_, err := FromRundeck("prod")(zipped, testNow)
	if err == nil {
		t.Fatal("FromRundeck() error = nil, want a zip that is not a project archive refused")
	}
	if !strings.Contains(err.Error(), "not a Rundeck project archive") {
		t.Errorf("error = %v, want it to say the zip is not a Rundeck project archive", err)
	}
}

// TestRundeckArchiveEmptyOfJobs proves an archive that parses and yields nothing is a refusal rather
// than a plan of zeros. An operator who exported the wrong project must not be told their migration
// succeeded with nothing to show for it.
func TestRundeckArchiveEmptyOfJobs(t *testing.T) {
	t.Parallel()
	entries := rundeckArchiveEntries()
	delete(entries, "rundeck-payments-batch/jobs/job-1.xml")
	_, err := FromRundeck("prod")(buildZip(t, entries), testNow)
	if !errors.Is(err, ErrNothingRecognized) {
		t.Errorf("error = %v, want ErrNothingRecognized", err)
	}
}

// TestRundeckArchiveReportsUnreadableJobs proves a job document this cannot read is named rather
// than passed over. A job silently missing from a migration is discovered when it does not run.
func TestRundeckArchiveReportsUnreadableJobs(t *testing.T) {
	t.Parallel()
	entries := rundeckArchiveEntries()
	entries["rundeck-payments-batch/jobs/job-2.xml"] = "<joblist><job><name>broken</name>"
	entries["rundeck-payments-batch/jobs/job-3.yaml"] = "- name: elsewhere\n"
	entries["rundeck-payments-batch/jobs/job-4.xml"] = "<project><builders/></project>"
	plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	wantWarning(t, plan, "rundeck-payments-batch/jobs/job-2.xml was not read")
	wantWarning(t, plan, "rundeck-payments-batch/jobs/job-3.yaml under the project's jobs directory")
	wantWarning(t, plan, "rundeck-payments-batch/jobs/job-4.xml was not read")
	// The one readable job still imports: an archive holding a bad document is a partial import
	// with the loss named, not a failed one.
	if diff := cmp.Diff([]string{"batch/close-books"}, templateNames(plan)); diff != "" {
		t.Errorf("templates mismatch (-want +got):\n%s", diff)
	}
}

// TestRundeckArchiveIgnoresExecutionHistory proves the executions, reports, and state files an
// archive carries are never read as jobs.
//
// Those are the files that tempt a reconstruction: a succeeded node list names hosts, so an importer
// could assemble an inventory out of the machines that happened to run. It would be wrong on every
// count that matters, since the list carries no address, no login, and nothing about the nodes that
// did not run, so this pins that they contribute nothing at all.
func TestRundeckArchiveIgnoresExecutionHistory(t *testing.T) {
	t.Parallel()
	entries := rundeckArchiveEntries()
	entries["rundeck-payments-batch/executions/execution-1.xml"] = `<executions><execution>` +
		`<succeededNodeList>pay-worker-01,pay-worker-02</succeededNodeList></execution></executions>`
	entries["rundeck-payments-batch/reports/report-1.xml"] = `<report><node>pay-worker-01</node></report>`
	entries["rundeck-payments-batch/jobfiles/upload-1.xml"] = `<joblist><job><name>not-a-job</name>` +
		`<sequence><command><exec>echo no</exec></command></sequence></job></joblist>`
	plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if diff := cmp.Diff([]string{"batch/close-books"}, templateNames(plan)); diff != "" {
		t.Errorf("templates mismatch (-want +got):\n%s", diff)
	}
	if len(plan.Inventories) != 0 {
		t.Errorf("inventories = %d, want none built from execution history", len(plan.Inventories))
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "pay-worker") {
			t.Errorf("a host name from the execution history reached the report: %s", w)
		}
	}
}

// TestRundeckArchiveScheduleShapes covers the schedule spellings an archive writes, which are not
// the ones a job export writes.
//
// Rundeck 5 folds the day of month onto the month element and leaves an empty dayofmonth element as
// the marker, so reading only the obvious element turned every monthly job into a daily one. The
// crontab attribute and the Quartz weekday numbering are covered here too, since the archive is the
// artifact most people will hand over and a schedule that fires on the wrong day is the failure
// nobody notices for a month.
func TestRundeckArchiveScheduleShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Schedule string
		WantCron string
	}{{ // Test 0: A daily job, with the day of month folded onto the month element.
		Name: "daily",
		Schedule: `<schedule><dayofmonth /><month day='*' month='*' />` +
			`<time hour='2' minute='30' seconds='0' /><year year='*' /></schedule>`,
		WantCron: "30 2 * * *",
	}, { // Test 1: A monthly job, whose day of month lives only on the month element.
		Name: "day of month",
		Schedule: `<schedule><dayofmonth /><month day='15' month='*' />` +
			`<time hour='4' minute='5' seconds='0' /><year year='*' /></schedule>`,
		WantCron: "5 4 15 * *",
	}, { // Test 2: A weekly job, whose Quartz weekday is renumbered for cron.
		Name: "weekday",
		Schedule: `<schedule><month month='*' /><time hour='6' minute='0' seconds='0' />` +
			`<weekday day='2' /><year year='*' /></schedule>`,
		WantCron: "0 6 * * 1",
	}, { // Test 3: A Quartz expression written as an attribute rather than as elements.
		Name:     "crontab attribute",
		Schedule: `<schedule crontab='0 15 3 ? * 6 *' />`,
		WantCron: "15 3 * * 5",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := rundeckArchiveEntries()
			entries["rundeck-payments-batch/jobs/job-1.xml"] = "<joblist><job><name>close-books</name>" +
				"<group>batch</group>" + test.Schedule +
				"<sequence><command><exec>echo hi</exec></command></sequence></job></joblist>"
			plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
			if err != nil {
				t.Fatalf("FromRundeck() error = %v", err)
			}
			if diff := cmp.Diff(map[string]string{"batch/close-books": test.WantCron},
				scheduleSpecs(plan)); diff != "" {
				t.Errorf("schedules mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRundeckArchiveOptionShapes covers the option spellings an archive writes, whose settings are
// attributes and whose allowed values are one delimited string rather than a list. A secure option
// is refused here exactly as it is on the job export path, since a survey answer is stored in plain
// text on every run and downgrading a password without saying so is worse than not importing it.
func TestRundeckArchiveOptionShapes(t *testing.T) {
	t.Parallel()
	entries := rundeckArchiveEntries()
	entries["rundeck-payments-batch/jobs/job-1.xml"] = `<joblist><job><name>close-books</name>` +
		`<group>batch</group><context><options preserveOrder='true'>` +
		`<option name='env' required='true' value='staging' values='staging|prod' ` +
		`valuesListDelimiter='|' enforcedvalues='true'><description>Where</description></option>` +
		`<option name='token' secure='true' valueExposed='false' />` +
		`<option name='tags' multivalued='true' /></options></context>` +
		`<sequence><command><exec>echo hi</exec></command></sequence></job></joblist>`
	plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	want := []template.SurveyField{{
		Var: "env", Label: "env", Type: template.FieldChoice, Required: true,
		Help: "Where", Default: "staging", Choices: []string{"staging", "prod"},
	}, {
		Var: "tags", Label: "tags", Type: template.FieldText,
	}}
	if diff := cmp.Diff(want, findTemplate(t, plan, "batch/close-books").Survey,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}
	wantWarning(t, plan, `option "token" is a secure option and was NOT imported`)
	wantWarning(t, plan, `option "tags" accepted several values at once`)
}

// TestRundeckArchiveNonShellScriptStep proves an inline script fed to something other than a shell is
// reported and left out rather than pasted into a Bash template.
//
// Rundeck writes the script body to a file and runs whatever the step names against it, so a step
// naming python3 holds Python source. Inlining that into the one Bash script a template runs would
// keep the job's name on something that cannot do what the job did.
func TestRundeckArchiveNonShellScriptStep(t *testing.T) {
	t.Parallel()
	entries := rundeckArchiveEntries()
	entries["rundeck-payments-batch/jobs/job-1.xml"] = `<joblist><job><name>close-books</name>` +
		`<group>batch</group><sequence><command><exec>echo start</exec></command>` +
		`<command><script>import os&#10;os.system("true")</script>` +
		`<scriptinterpreter>/usr/bin/python3</scriptinterpreter></command></sequence>` +
		`</job></joblist>`
	plan, err := FromRundeck("prod")(buildZip(t, entries), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	wantWarning(t, plan, `feeds its script to "/usr/bin/python3" rather than to a shell`)
	if got := findTemplate(t, plan, "batch/close-books").Command; strings.Contains(got, "import os") {
		t.Errorf("command = %q, want the Python step left out of the Bash script", got)
	}
}

// TestJobExportStillReadsAsItself proves the job export path is untouched by the archive path.
//
// Detection is by content, and the two artifacts share one entry point, so the way to break the
// existing importer is to have a document take the archive branch or lose a field the archive shape
// does not carry. A YAML export, a JSON export, and an export whose first bytes could be mistaken for
// something else all still land as they did.
func TestJobExportStillReadsAsItself(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Export   string
		WantName string
	}{{ // Test 0: The YAML shape, which is what Rundeck's job list exports.
		Name: "yaml", WantName: "batch/close",
		Export: "- name: close\n  group: batch\n  sequence:\n    commands:\n      - exec: echo hi\n",
	}, { // Test 1: The JSON shape, which the API returns and which is also valid YAML.
		Name: "json", WantName: "batch/close",
		Export: `[{"name":"close","group":"batch","sequence":{"commands":[{"exec":"echo hi"}]}}]`,
	}, { // Test 2: The wrapped shape, whose job list sits under a key.
		Name: "wrapped", WantName: "close",
		Export: "jobs:\n  - name: close\n    sequence:\n      commands:\n        - exec: echo hi\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if IsRundeckArchive([]byte(test.Export)) {
				t.Fatal("IsRundeckArchive() = true for a job export document")
			}
			plan, err := FromRundeck("prod")([]byte(test.Export), testNow)
			if err != nil {
				t.Fatalf("FromRundeck() error = %v", err)
			}
			if diff := cmp.Diff([]string{test.WantName}, templateNames(plan)); diff != "" {
				t.Errorf("templates mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseJavaProperties covers the format Rundeck writes its project and SCM configuration in.
//
// It is not key=value with the rest of the line as the value. Rundeck escapes the colon in every
// URL, so reading a line as written yields a repository URL no git can dial and a node source
// endpoint nobody could call. The rest of the format's rules are here because a configuration file
// is somebody else's file and the parser meets whatever it holds.
func TestParseJavaProperties(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		In    string
		WantK string
		WantV string
	}{{ // Test 0: The escaped colon Rundeck writes into every URL.
		Name: "escaped colon", In: `scm.export.config.url=file\:///home/rundeck/jobs.git`,
		WantK: "scm.export.config.url", WantV: "file:///home/rundeck/jobs.git",
	}, { // Test 1: The same escape in an https URL, which is the node source case.
		Name: "escaped https", In: `resources.source.1.config.url=https\://cmdb.example.com/n.yaml`,
		WantK: "resources.source.1.config.url", WantV: "https://cmdb.example.com/n.yaml",
	}, { // Test 2: A colon separator, which the format allows in place of the equals sign.
		Name: "colon separator", In: "project.name:payments-batch",
		WantK: "project.name", WantV: "payments-batch",
	}, { // Test 3: A whitespace separator, which the format also allows.
		Name: "space separator", In: "project.name payments-batch",
		WantK: "project.name", WantV: "payments-batch",
	}, { // Test 4: Whitespace around an equals sign, which belongs to neither side.
		Name: "padded equals", In: "project.name = payments-batch",
		WantK: "project.name", WantV: "payments-batch",
	}, { // Test 5: A line continued onto the next, which is one value rather than two lines.
		Name: "continuation", In: "project.description=Nightly payments \\\n    batch runners",
		WantK: "project.description", WantV: "Nightly payments batch runners",
	}, { // Test 6: An escaped backslash, which ends the line rather than continuing it.
		Name: "escaped backslash", In: `project.ssh-keypath=C\:\\keys\\id_rsa`,
		WantK: "project.ssh-keypath", WantV: `C:\keys\id_rsa`,
	}, { // Test 7: A unicode escape, which the format spells the Java way.
		Name: "unicode escape", In: `project.description=caf\u00e9`,
		WantK: "project.description", WantV: "café",
	}, { // Test 8: A key with no value, which is a key with an empty value rather than a parse error.
		Name: "bare key", In: "scm.export.enabled",
		WantK: "scm.export.enabled", WantV: "",
	}, { // Test 9: An escaped separator in the key, which does not end the key.
		Name: "escaped key", In: `weird\=key=value`, WantK: "weird=key", WantV: "value",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			// The comment forms and a blank line are prepended to every case, since a parser that
			// read one of them as a setting would put a key nobody wrote into the configuration.
			body := "#Exported configuration\n!bang comment\n\n" + test.In + "\n"
			got := parseJavaProperties([]byte(body))
			want := map[string]string{test.WantK: test.WantV}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("properties mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseManifest covers the JAR manifest a project archive names itself in, including the wrapped
// line a long project name produces.
func TestParseManifest(t *testing.T) {
	t.Parallel()
	got := parseManifest([]byte("Manifest-Version: 1.0\r\n" +
		"Rundeck-Archive-Format-Version: 1.0\r\n" +
		"Rundeck-Archive-Project-Name: payments-\r\n batch\r\n\r\n"))
	want := map[string]string{
		"Manifest-Version":               "1.0",
		"Rundeck-Archive-Format-Version": "1.0",
		"Rundeck-Archive-Project-Name":   "payments-batch",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("manifest mismatch (-want +got):\n%s", diff)
	}
}
