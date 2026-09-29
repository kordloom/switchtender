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
