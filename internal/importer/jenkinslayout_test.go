package importer_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/importer"
)

// Root elements of the job types a real jobs directory holds beside freestyle jobs.
const (
	folderDoc      = "<com.cloudbees.hudson.plugins.folder.Folder/>"
	multibranchDoc = "<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject/>"
	promotionDoc   = "<hudson.plugins.promoted__builds.PromotionProcess/>"
)

// writeJenkinsTree writes a path-to-content map under dir, so a walk can be pointed at it.
func writeJenkinsTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("make fixture: %v", err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
}

// TestJenkinsZipReadsAJobOnlyWhereTheWalkWould pins the layout rule the archive reader shares with
// the directory walk, by reading each tree both as a zip and from disk. Every real jobs directory
// holds files named config.xml that are not jobs: archived artifacts, a workspace checkout, a
// multibranch project's branches, a promotion. Reading one as a job reports a job that does not
// exist, and an archived artifact larger than a definition refuses the whole archive. An archive
// zipped from inside the jobs directory has no leading jobs segment, so a jobs tree inside an
// artifact or a workspace is the first one the reader meets and must still anchor nothing.
func TestJenkinsZipReadsAJobOnlyWhereTheWalkWould(t *testing.T) {
	t.Parallel()
	job := freestyle(shellStep("echo hi"))
	tests := []struct {
		Files     map[string]string
		WantNames []string
	}{{ // Test 0: An archived artifact named config.xml, larger than a definition may be.
		Files: map[string]string{
			"jobs/simple/config.xml":                           job,
			"jobs/simple/builds/12/archive/app/config.xml":     strings.Repeat("x", 5<<20),
			"jobs/simple/builds/12/archive/app/bin/config.xml": "",
		},
		WantNames: []string{"simple"},
	}, { // Test 1: A workspace checkout and a promotion, each carrying a config.xml.
		Files: map[string]string{
			"jobs/simple/config.xml":                    job,
			"jobs/simple/workspace/config.xml":          "<config/>",
			"jobs/simple/promotions/release/config.xml": promotionDoc,
		},
		WantNames: []string{"simple"},
	}, { // Test 2: A multibranch project's branches are not jobs of their own.
		Files: map[string]string{
			"jobs/mb/config.xml":                    multibranchDoc,
			"jobs/mb/branches/main/config.xml":      "<flow-definition/>",
			"jobs/mb/branches/feature-x/config.xml": "<flow-definition/>",
		},
		WantNames: []string{"mb"},
	}, { // Test 3: Zipped from inside the jobs directory, with no jobs segment to anchor on.
		Files: map[string]string{
			"simple/config.xml":                  job,
			"simple/builds/1/archive/config.xml": "<config/>",
			"simple/workspace/config.xml":        "<config/>",
		},
		WantNames: []string{"simple"},
	}, { // Test 4: A folder's children still arrive, through its jobs directory.
		Files: map[string]string{
			"jobs/platform/config.xml":                              folderDoc,
			"jobs/platform/jobs/vacuum/config.xml":                  job,
			"jobs/platform/jobs/vacuum/workspace/config.xml":        "<config/>",
			"jobs/platform/jobs/vacuum/builds/3/archive/config.xml": "<config/>",
		},
		WantNames: []string{"platform/vacuum"},
	}, { // Test 5: Zipped from inside jobs, with a jobs tree archived by a backup job's build.
		Files: map[string]string{
			"backup/config.xml": job,
			"deploy/config.xml": job,
			"backup/builds/7/archive/jobs/deploy/config.xml": job,
			"backup/builds/7/archive/jobs/other/config.xml":  job,
		},
		WantNames: []string{"backup", "deploy"},
	}, { // Test 6: One job's directory whose workspace checkout holds a jobs tree.
		Files: map[string]string{
			"deploy/config.xml":                     job,
			"deploy/workspace/jobs/seed/config.xml": job,
		},
		WantNames: []string{"deploy"},
	}, { // Test 7: An archived jobs tree carrying a config.xml larger than a definition may be.
		Files: map[string]string{
			"backup/config.xml":                           job,
			"backup/builds/7/archive/jobs/big/config.xml": strings.Repeat("x", 5<<20),
		},
		WantNames: []string{"backup"},
	}, { // Test 8: The same archived tree below a leading jobs directory.
		Files: map[string]string{
			"jobs/backup/config.xml":                              job,
			"jobs/backup/builds/7/archive/jobs/deploy/config.xml": job,
		},
		WantNames: []string{"backup"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bundle, err := importer.JenkinsBundleFromZip(zipOf(t, test.Files))
			if err != nil {
				t.Fatalf("JenkinsBundleFromZip() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNames, importer.JenkinsJobNames(bundle),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("zip names mismatch (-want +got):\n%s", diff)
			}
			dir := t.TempDir()
			writeJenkinsTree(t, dir, test.Files)
			walked, err := importer.JenkinsBundle(dir)
			if err != nil {
				t.Fatalf("JenkinsBundle() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNames, importer.JenkinsJobNames(walked),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("walk names mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestJenkinsRefusedOnlyExportStillReports pins that a Jenkins holding nothing but jobs this
// refuses by name still produces the report. Most Jenkins estates today are Pipeline and
// multibranch, and for one of those the reasons each job does not come across are the answer,
// where a refusal calling the export unrecognized would hide every one of them.
func TestJenkinsRefusedOnlyExportStillReports(t *testing.T) {
	t.Parallel()
	plan, err := importer.FromJenkins("prod")(jenkinsBundle(
		[2]string{"pipe", "<flow-definition><definition/></flow-definition>"},
		[2]string{"mb", multibranchDoc},
		[2]string{"odd", "<com.example.SomethingElse/>"},
	), fixedTime)
	if errors.Is(err, importer.ErrNothingRecognized) {
		t.Fatalf("FromJenkins() = %v, want the plan naming each refused job instead", err)
	}
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Templates) != 0 {
		t.Errorf("templates = %+v, want none from an export this refuses whole", plan.Templates)
	}
	report := plan.Report()
	if report.CreatedTotal != 0 || len(report.LeftOut) != 3 {
		t.Errorf("report = %+v, want nothing created and three jobs left out", report)
	}
	assertWarns(t, plan.Warnings, `job "pipe" is a Pipeline job`, `job "mb" is a multibranch`,
		`job "odd" has the unrecognized type`)
}

// TestJenkinsUnreadableOnlyExportStillReports pins that a Jenkins whose only named jobs cannot be
// read gets the report naming each one and what is wrong with it, while a lone unnamed document
// that cannot be read is still called unrecognized, since it is most likely the wrong file.
func TestJenkinsUnreadableOnlyExportStillReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Jobs         [][2]string
		WantLeftOut  int
		WantWarnings []string
		Want         error
	}{{ // Test 0: A named job whose disabled flag is not a boolean.
		Jobs:         [][2]string{{"deploy", "<project><disabled>maybe</disabled></project>"}},
		WantLeftOut:  1,
		WantWarnings: []string{`job "deploy" could not be read`},
	}, { // Test 1: A named job with no document inside it.
		Jobs:         [][2]string{{"deploy", ""}},
		WantLeftOut:  1,
		WantWarnings: []string{`job "deploy" could not be read`},
	}, { // Test 2: Two named jobs, each malformed in its own way.
		Jobs: [][2]string{
			{"deploy", "<project><disabled>maybe</disabled></project>"}, {"backup", ""},
		},
		WantLeftOut:  2,
		WantWarnings: []string{`job "deploy" could not be read`, `job "backup" could not be read`},
	}, { // Test 3: A lone unnamed document that cannot be read.
		Jobs: [][2]string{{"", "<project><disabled>maybe</disabled></project>"}},
		Want: importer.ErrNothingRecognized,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := importer.FromJenkins("prod")(jenkinsBundle(test.Jobs...), fixedTime)
			if !errors.Is(err, test.Want) {
				t.Fatalf("FromJenkins() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			report := plan.Report()
			if diff := cmp.Diff(test.WantLeftOut, len(report.LeftOut)); diff != "" {
				t.Errorf("left out mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(0, report.CreatedTotal); diff != "" {
				t.Errorf("created mismatch (-want +got):\n%s", diff)
			}
			assertWarns(t, plan.Warnings, test.WantWarnings...)
		})
	}
}

// TestJenkinsWrongDocumentIsUnrecognized pins that a document which is not a Jenkins job is refused
// as unrecognized rather than previewed as one. A pasted JENKINS_HOME config.xml, another tool's
// export, or a feed is most likely the wrong file, and the refusal tells the reader to check the
// export, where a plan would call the document a job of an unrecognized type. A job of a type
// refused by name, or a named job of an unknown type, still produces the report.
func TestJenkinsWrongDocumentIsUnrecognized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Doc         []byte
		Want        error
		WantWarning string
	}{{ // Test 0: The controller's own configuration, pasted on its own.
		Doc:  []byte("<hudson><mode>NORMAL</mode></hudson>"),
		Want: importer.ErrNothingRecognized,
	}, { // Test 1: The same, behind the XML 1.1 declaration Jenkins writes.
		Doc: []byte("<?xml version='1.1' encoding='UTF-8'?>\n" +
			"<hudson><mode>NORMAL</mode></hudson>"),
		Want: importer.ErrNothingRecognized,
	}, { // Test 2: A Rundeck job list sent to the Jenkins importer.
		Doc:  []byte("<joblist><job><name>x</name></job></joblist>"),
		Want: importer.ErrNothingRecognized,
	}, { // Test 3: A document that is no job definition at all.
		Doc:  []byte(`<?xml version="1.0"?><rss><channel/></rss>`),
		Want: importer.ErrNothingRecognized,
	}, { // Test 4: The controller's configuration named by its directory, as the CLI hands it over.
		Doc:  jenkinsBundle([2]string{"jenkins_home", "<hudson><mode>NORMAL</mode></hudson>"}),
		Want: importer.ErrNothingRecognized,
	}, { // Test 5: A lone Pipeline job is refused by name and reported.
		Doc:         []byte("<flow-definition><definition/></flow-definition>"),
		WantWarning: "is a Pipeline job",
	}, { // Test 6: A job a directory named, of a type nothing recognizes, is reported.
		Doc:         jenkinsBundle([2]string{"odd", "<com.example.SomethingElse/>"}),
		WantWarning: `job "odd" has the unrecognized type`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := importer.FromJenkins("prod")(test.Doc, fixedTime)
			if !errors.Is(err, test.Want) {
				t.Fatalf("FromJenkins() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			assertWarns(t, plan.Warnings, test.WantWarning)
		})
	}
}

// TestJenkinsBundleReadsASingleJobDirectory pins that a job's directory imports as that one job,
// named after the directory. Its config.xml sits in the directory itself, and nothing under it is
// read, so a workspace checkout carrying a file by that name never becomes a job named workspace.
func TestJenkinsBundleReadsASingleJobDirectory(t *testing.T) {
	t.Parallel()
	job := freestyle(shellStep("echo hi"))
	tests := []struct {
		Files     map[string]string
		WantNames []string
	}{{ // Test 0: A job directory holding only its definition.
		Files: map[string]string{"config.xml": job}, WantNames: []string{"deploy"},
	}, { // Test 1: A job directory with its build history and a workspace carrying a config.xml.
		Files: map[string]string{
			"config.xml": job, "builds/1/log": "noise", "builds/1/build.xml": "<build/>",
			"workspace/config.xml": "<config/>", "promotions/release/config.xml": promotionDoc,
		},
		WantNames: []string{"deploy"},
	}, { // Test 2: A folder is entered through its jobs directory, as before.
		Files:     map[string]string{"config.xml": folderDoc, "jobs/child/config.xml": job},
		WantNames: []string{"child"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "deploy")
			writeJenkinsTree(t, dir, test.Files)
			bundle, err := importer.JenkinsBundle(dir)
			if err != nil {
				t.Fatalf("JenkinsBundle() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNames, importer.JenkinsJobNames(bundle),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("names mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestArchiveRefusalsAreMarked pins that each archive reader's refusal matches ErrArchive, which
// is how the server tells a sentence written for the person who built the archive from a failure
// of its own, and passes it through instead of a generic line about the format.
func TestArchiveRefusalsAreMarked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Mapper   func([]byte, time.Time) (*importer.Plan, error)
		Files    map[string]string
		WantText string
	}{{ // Test 0: A Jenkins archive whose only config.xml has no directory naming it.
		Name: "jenkins unnamed", Mapper: importer.FromJenkins(""),
		Files: map[string]string{"config.xml": "<project/>"}, WantText: "naming its job",
	}, { // Test 1: A Jenkins archive with no job definition in it at all.
		Name: "jenkins empty", Mapper: importer.FromJenkins(""),
		Files: map[string]string{"jobs/readme.txt": "nothing"}, WantText: "no config.xml found",
	}, { // Test 2: Zipped inside one job's directory, whose workspace holds a config.xml.
		Name: "jenkins one job's workspace", Mapper: importer.FromJenkins(""),
		Files: map[string]string{
			"config.xml": freestyle(shellStep("echo hi")), "workspace/config.xml": "<config/>",
		},
		WantText: "naming its job",
	}, { // Test 3: The same for a Pipeline job, with an archived artifact as well.
		Name: "jenkins one pipeline's workspace", Mapper: importer.FromJenkins(""),
		Files: map[string]string{
			"config.xml": "<flow-definition/>", "workspace/config.xml": "<config/>",
			"builds/1/archive/config.xml": "<config/>",
		},
		WantText: "naming its job",
	}, { // Test 4: A Rundeck archive carrying a member no export writes.
		Name: "rundeck absolute path", Mapper: importer.FromRundeck(""),
		Files: map[string]string{"/etc/passwd": "root:x:0:0"}, WantText: "absolute path",
	}, { // Test 5: A Rundeck archive that is not a project archive.
		Name: "rundeck not a project", Mapper: importer.FromRundeck(""),
		Files: map[string]string{"notes/readme.txt": "nothing"}, WantText: "not a Rundeck project",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := test.Mapper(zipOf(t, test.Files), fixedTime)
			if !errors.Is(err, importer.ErrArchive) {
				t.Fatalf("error = %v, want one matching ErrArchive", err)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("error = %v, want it to say %q", err, test.WantText)
			}
		})
	}
}
