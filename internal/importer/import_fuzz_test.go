package importer

import (
	"archive/zip"
	"bytes"
	"testing"
	"time"
)

// fuzzNow is a fixed time so the fuzzers stay deterministic.
var fuzzNow = time.Unix(0, 0).UTC()

// FuzzFromAWX feeds arbitrary bytes to the AWX importer to prove it never panics on a malformed or
// hostile export.
func FuzzFromAWX(f *testing.F) {
	f.Add([]byte(`{"projects":[],"inventories":[],"job_templates":[]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromAWX(data, fuzzNow)
	})
}

// FuzzFromSemaphore feeds arbitrary bytes to the Semaphore importer to prove it never panics on a
// malformed or hostile export.
func FuzzFromSemaphore(f *testing.F) {
	f.Add([]byte(`{"templates":[],"repositories":[]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromSemaphore(data, fuzzNow)
	})
}

// FuzzRRULEToCron feeds arbitrary recurrence strings to the schedule converter to prove it never
// panics on a malformed rule.
func FuzzRRULEToCron(f *testing.F) {
	f.Add("FREQ=DAILY;INTERVAL=1")
	f.Add("FREQ=WEEKLY;BYDAY=MO,WE,FR")
	f.Add("")
	f.Fuzz(func(_ *testing.T, s string) {
		_, _ = RRULEToCron(s)
	})
}

// rundeckArchiveSeed builds a small project archive for the Rundeck fuzzer's corpus, so mutations
// reach the archive reader rather than only the job export decoder.
func rundeckArchiveSeed(f *testing.F) []byte {
	f.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	entries := map[string]string{
		"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\nRundeck-Archive-Project-Name: p\n\n",
		"rundeck-p/files/etc/project.properties": "resources.source.1.type=url\n" +
			"resources.source.1.config.url=https\\://c.example.com/n.yaml\n",
		"rundeck-p/files/etc/scm-export.properties": "scm.export.config.url=file\\:///r/j.git\n",
		"rundeck-p/jobs/job-1.xml": "<joblist><job><name>a</name><schedule crontab='0 0 2 * * ? *'/>" +
			"<sequence><command><exec>x</exec></command></sequence></job></joblist>",
	}
	for name, body := range entries {
		e, err := w.Create(name)
		if err != nil {
			f.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := e.Write([]byte(body)); err != nil {
			f.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		f.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// FuzzFromRundeck feeds arbitrary bytes to the Rundeck importer to prove it never panics on a
// malformed or hostile job export. The Quartz schedule converter walks the weekday field character
// by character, so a hostile expression is worth reaching from here.
//
// A project archive is worth reaching too, and from the same entry point, since the artifact is
// chosen by content. It opens a zip decoder, a properties parser that walks backslash escapes byte
// by byte, and an XML decoder, all on a file somebody else wrote and uploaded.
func FuzzFromRundeck(f *testing.F) {
	f.Add(rundeckArchiveSeed(f))
	f.Add([]byte(`- name: a
  sequence:
    commands:
      - exec: x`))
	f.Add([]byte(`- name: a
  schedule:
    crontab: '0 0 2 * * ? *'`))
	f.Add([]byte(`- name: a
  schedule:
    crontab: '0 0 0 0 0 0 0'`))
	f.Add([]byte(`jobs: [{"name":"a","options":[{"name":"p","secure":true}]}]`))
	f.Add([]byte(`[`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromRundeck("hosts")(data, fuzzNow)
	})
}

// FuzzFromJenkins feeds arbitrary bytes to the Jenkins importer to prove it never panics on a
// malformed or hostile job configuration.
//
// The H notation converter is the reason this matters more than the shape of the XML. It slices
// terms apart by index to find a bracketed window and a step, and it walks the weekday field byte by
// byte looking at neighbors, so a truncated bracket, an enormous step, or a term that is nothing but
// the letter H all reach arithmetic that has no business trusting its input.
func FuzzFromJenkins(f *testing.F) {
	seed := func(spec string) []byte {
		return []byte(`<jobs><job name="a"><project><triggers><hudson.triggers.TimerTrigger><spec>` +
			spec + `</spec></hudson.triggers.TimerTrigger></triggers><builders>` +
			`<hudson.tasks.Shell><command>x</command></hudson.tasks.Shell></builders>` +
			`</project></job></jobs>`)
	}
	f.Add(seed("H 2 * * *"))
	f.Add(seed("H("))
	f.Add(seed("H(0-"))
	f.Add(seed("H(9-0) * * * *"))
	f.Add(seed("H(-1--2) * * * *"))
	f.Add(seed("H/0 * * * *"))
	f.Add(seed("H/-5 * * * *"))
	f.Add(seed("H/99999999999999999999 * * * *"))
	f.Add(seed("H(0-2)/0 * * * *"))
	f.Add(seed("* * * * 7777777"))
	f.Add(seed("HHHHH HHHHH HHHHH HHHHH HHHHH"))
	f.Add(seed("@daily"))
	f.Add(seed("@"))
	f.Add(seed(",,,, * * * *"))
	f.Add([]byte(`<jobs><job name="a"><flow-definition/></job></jobs>`))
	f.Add([]byte(`<jobs><job name=""></job></jobs>`))
	f.Add([]byte(`<project><builders><hudson.tasks.Shell><command>$WORKSPACE</command>` +
		`</hudson.tasks.Shell></builders></project>`))
	f.Add([]byte(`<jobs>`))
	f.Add([]byte(`<`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromJenkins("inv")(data, fuzzNow)
	})
}

// FuzzFromChef feeds arbitrary bytes to the Chef importer to prove it never panics on a malformed
// or hostile node export.
//
// The importer accepts three shapes, an array of node documents, one node document, and an object
// keyed by node name, and chooses between them by what decodes rather than by what the caller
// says. That branching is exactly what a fuzzer is good at surprising, and the run-list parser
// splits on brackets by index, which is the other place a hostile export gets to pick the offsets.
func FuzzFromChef(f *testing.F) {
	f.Add([]byte(`[{"name":"web01","chef_environment":"production","run_list":["role[base]"]}]`))
	f.Add([]byte(`{"web01":{"chef_environment":"production","run_list":["recipe[nginx]"]}}`))
	f.Add([]byte(`{"name":"solo","automatic":{"ipaddress":"10.0.0.1","fqdn":"solo.prod"}}`))
	f.Add([]byte(`[{"name":"a","run_list":["role["]}]`))
	f.Add([]byte(`[{"name":"a","run_list":["]"]}]`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromChef(data, fuzzNow)
	})
}

// FuzzFromPuppet feeds arbitrary bytes to the Puppet importer to prove it never panics.
//
// It reads three shapes as well: a PuppetDB nodes query, a facts query that names its nodes one
// fact row at a time, and the plain certname list `puppet node list` prints, which is not JSON at
// all. A fuzzer gets to hand it something that is almost each of those.
func FuzzFromPuppet(f *testing.F) {
	f.Add([]byte(`[{"certname":"a.prod","catalog_environment":"production"}]`))
	f.Add([]byte(`[{"certname":"b.prod","name":"ipaddress","value":"10.0.0.2"}]`))
	f.Add([]byte("c.prod\nd.prod\n\n# comment\n"))
	f.Add([]byte(`[{"certname":"x","deactivated":"2026-08-01T00:00:00Z"}]`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = FromPuppet(data, fuzzNow)
	})
}

// FuzzFromCron feeds arbitrary bytes to the crontab importer to prove it never panics.
//
// Both forms are exercised, because the six-field `/etc/crontab` shape carries a user column
// before the command and is parsed by position: a short line is the obvious way to walk off the
// end of the fields, and only the system form looks that far.
func FuzzFromCron(f *testing.F) {
	f.Add([]byte("0 3 * * * /usr/bin/backup.sh\n"))
	f.Add([]byte("0 3 * * * root /usr/bin/backup.sh\n"))
	f.Add([]byte("* * * * *\n"))
	f.Add([]byte("@reboot /bin/true\n"))
	f.Add([]byte("MAILTO=ops\n0 1 * * 1-5 /bin/thing\n"))
	f.Add([]byte("* * * *\n"))
	f.Add([]byte(""))
	f.Fuzz(func(_ *testing.T, data []byte) {
		for _, system := range []bool{false, true} {
			_, _ = FromCron("hosts.ini", system)(data, fuzzNow)
		}
	})
}
