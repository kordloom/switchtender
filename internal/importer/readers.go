package importer

import (
	"sort"
	"time"
)

// Readers maps a format name to the function that reads an export of it.
//
// One registry, because two callers reach for it and they must offer the same list: the assess
// command and the browser assessment. A format the page offered and the command did not, or the
// reverse, would be the same failure the shared reader exists to prevent: somebody assessing an
// estate with one tool and getting a different answer from the other. The import command and the
// import API read each of these formats through the same functions, so what an assessment says
// comes across is what an import brings.
//
// An AWX-format export also covers Ansible Automation Platform, Tower, and Ascender.
var Readers = map[string]func([]byte, time.Time) (*Plan, error){
	"awx":       FromAWX,
	"semaphore": FromSemaphore,
	"chef":      FromChef,
	"puppet":    FromPuppet,
}

// Formats returns the readable format names in a stable order, for listing them to somebody who
// named one that does not exist.
func Formats() []string {
	out := make([]string, 0, len(Readers))
	for name := range Readers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
