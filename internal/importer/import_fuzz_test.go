package importer

import (
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

// FuzzFromRundeck feeds arbitrary bytes to the Rundeck importer to prove it never panics on a
// malformed or hostile job export. The Quartz schedule converter walks the weekday field character
// by character, so a hostile expression is worth reaching from here.
func FuzzFromRundeck(f *testing.F) {
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
