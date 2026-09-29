package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The harness binary is standard library only and imports nothing from the rest of the module, so
// that a supertest run depends on the product and on nothing else. Test files are not compiled into
// that binary, so the comparison helper here costs the guarantee nothing.

// tableRows returns the report's check rows, which are the lines between the header separator and
// the blank line that ends the table.
func tableRows(report string) []string {
	var rows []string
	inTable := false
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "|---") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		rows = append(rows, line)
	}
	return rows
}

// unescapedPipes counts the cell separators in a row, which is every pipe not preceded by a
// backslash. A four-column row has five of them and nothing else is a row.
func unescapedPipes(row string) int {
	n := 0
	for i := range len(row) {
		if row[i] != '|' {
			continue
		}
		if i > 0 && row[i-1] == '\\' {
			continue
		}
		n++
	}
	return n
}

// TestEveryCheckRendersAsExactlyOneRow holds the report to the shape a reader and a Markdown parser
// both require, whatever a phase put in a claim or its evidence.
//
// The report is the artifact an evaluator reads, and a run costs thirteen minutes, so a report that
// mangles a row is a run somebody has to repeat. The renderer guarded the failure path, where the
// evidence is an error, and left the passing path raw, so a claim that held with several lines of
// evidence behind it broke the table that was supposed to show it.
func TestEveryCheckRendersAsExactlyOneRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Checks []check
	}{{ // Test 0: Evidence for a claim that held, carrying the newlines a command wrote.
		Name: "multi-line detail on a pass",
		Checks: []check{{
			Phase: "community", Name: "a real Ansible run succeeded across the fleet",
			Detail: "host-a: ok\nhost-b: ok\nhost-c: ok",
		}},
	}, { // Test 1: The same, on a claim that did not hold.
		Name: "multi-line detail on a failure",
		Checks: []check{{
			Phase: "dr", Name: "the restore reported what it wrote",
			Err: fmt.Errorf("restore failed:\nkind: runs\nkind: receipts"),
		}},
	}, { // Test 2: A cell separator inside the claim rather than inside the evidence.
		Name: "pipe in the claim name",
		Checks: []check{{
			Phase: "team", Name: "the run was graded irreversible | not merely risky",
			Detail: "grade=irreversible",
		}},
	}, { // Test 3: A cell separator inside the phase label.
		Name: "pipe in the phase",
		Checks: []check{{
			Phase: "upgrade|rollback", Name: "the chain verifies across the boundary",
			Detail: "ok",
		}},
	}, { // Test 4: Both at once, which is what a real failure tail looks like.
		Name: "pipe and newline together",
		Checks: []check{{
			Phase: "ha", Name: "the chain is one unforked history | after the failover",
			Detail: "replica-a | head=9\nreplica-b | head=9",
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			h := &harness{checks: test.Checks}
			rows := tableRows(h.renderReport())
			if diff := cmp.Diff(len(test.Checks), len(rows)); diff != "" {
				t.Errorf("one row per check (-want +got):\n%s\nreport rows:\n%s",
					diff, strings.Join(rows, "\n"))
			}
			for i, row := range rows {
				if diff := cmp.Diff(5, unescapedPipes(row)); diff != "" {
					t.Errorf("row %d is not four cells (-want +got):\n%s\nrow: %q", i, diff, row)
				}
			}
		})
	}
}

// TestTheReportStatesWhichTiersActuallyRan keeps the header describing the run in front of it.
//
// A report claiming both tiers after a Community-only run is the harness doing the one thing it
// exists to forbid, which is asserting more than it saw.
func TestTheReportStatesWhichTiersActuallyRan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantContains string
		SkipTeam     bool
	}{{ // Test 0: Both tiers exercised.
		SkipTeam: false, WantContains: "both install tiers",
	}, { // Test 1: The Team tier was skipped and the report has to say so.
		SkipTeam: true, WantContains: "the Team tier was NOT exercised",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := &harness{skipTeam: test.SkipTeam}
			if got := h.renderReport(); !strings.Contains(got, test.WantContains) {
				t.Errorf("report does not say %q:\n%s", test.WantContains, got)
			}
		})
	}
}

// TestTheReportCountsWhatHeldAndWhatDidNot pins the tally and the section that carries a failure in
// full, since the table squeezes every failure to one line.
func TestTheReportCountsWhatHeldAndWhatDidNot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantTally    string
		Checks       []check
		WantFailures bool
	}{{ // Test 0: Nothing failed, so the full-failure section must not appear at all.
		Checks: []check{
			{Phase: "cluster", Name: "held", Detail: "ok"},
			{Phase: "fleet", Name: "also held", Detail: "ok"},
		},
		WantTally: "**2 checks, 0 failed.**", WantFailures: false,
	}, { // Test 1: One of three failed, and its error must appear in full below the table.
		Checks: []check{
			{Phase: "cluster", Name: "held", Detail: "ok"},
			{Phase: "dr", Name: "did not hold", Err: fmt.Errorf("restore empty")},
			{Phase: "ha", Name: "held", Detail: "ok"},
		},
		WantTally: "**3 checks, 1 failed.**", WantFailures: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := &harness{checks: test.Checks}
			got := h.renderReport()
			if !strings.Contains(got, test.WantTally) {
				t.Errorf("tally %q missing from:\n%s", test.WantTally, got)
			}
			hasSection := strings.Contains(got, "### Failures in full")
			if diff := cmp.Diff(test.WantFailures, hasSection); diff != "" {
				t.Errorf("failures section (-want +got):\n%s", diff)
			}
		})
	}
}
