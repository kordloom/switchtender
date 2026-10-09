package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tableBlock matches one HTML table on a page, with everything between its tags.
var tableBlock = regexp.MustCompile(`(?s)<table[^>]*>(.*?)</table>`)

// tableRow matches one row of a table, with everything between its tags.
var tableRow = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)

// tableCell matches the opening tag of a header or data cell.
var tableCell = regexp.MustCompile(`<t[hd][\s>]`)

// tag matches any HTML tag, for reducing a cell to its text.
var tag = regexp.MustCompile(`<[^>]+>`)

// sitePages returns every HTML page under site/, so a table on any of them is checked.
func sitePages(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "site")
	var pages []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		pages = append(pages, p)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(pages) < 20 {
		t.Fatalf("found %d pages under %s, so the walk did not read the site", len(pages), root)
	}
	return pages
}

// TestEveryTableRowHasAsManyCellsAsItsHeader fails for a row on any published page that has more
// or fewer cells than the header above it.
//
// The comparison tables are edited by hand, one long line per row, and a row with a cell missing
// renders without an error: the browser shifts every later cell one column left, so a vendor's
// answer appears under the wrong vendor's name. That is the one mistake a comparison page cannot
// afford, and nothing else on the site counts the cells.
func TestEveryTableRowHasAsManyCellsAsItsHeader(t *testing.T) {
	t.Parallel()
	tables := 0
	for _, page := range sitePages(t) {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		for _, table := range tableBlock.FindAllStringSubmatch(string(body), -1) {
			rows := tableRow.FindAllStringSubmatch(table[1], -1)
			if len(rows) < 2 {
				continue
			}
			tables++
			want := len(tableCell.FindAllString(rows[0][1], -1))
			for i, row := range rows[1:] {
				if got := len(tableCell.FindAllString(row[1], -1)); got != want {
					t.Errorf("%s: row %d of a table has %d cells under a header of %d: %q",
						page, i+1, got, want, rowLabel(row[1]))
				}
			}
		}
	}
	if tables == 0 {
		t.Fatal("no table was parsed on any page, so this test is not checking anything")
	}
}

// rowLabel returns the text of a row's first cell, which is the capability it names.
func rowLabel(row string) string {
	cells := strings.SplitN(row, "</td>", 2)
	if len(cells) < 2 {
		cells = strings.SplitN(row, "</th>", 2)
	}
	return strings.TrimSpace(tag.ReplaceAllString(cells[0], ""))
}

// frameRows are the capabilities the positioning rests on, in the order every comparison table
// opens with them. Each entry is matched case-insensitively anywhere in the row's label, since the
// docs table says "Ceiling on AI agents" where the landing pages say "Hard ceiling on AI agents".
var frameRows = []string{
	"Approval bound to the content that runs",
	"ceiling on AI agents",
	"Evidence a third party verifies offline",
}

// TestEveryComparisonTableOpensWithTheFrameRows pins the first three rows of each comparison
// table to the three capabilities the product is positioned on.
//
// The rest of each table is controller work, where the field is often even, and the three rows
// that are not even were buried below it or absent. The homepage, the five landing pages that
// compare against a product in the table, and the comparison doc each carry a table, and a row
// added to one of them was free to be left out of the others.
func TestEveryComparisonTableOpensWithTheFrameRows(t *testing.T) {
	t.Parallel()
	pages := []string{
		"index.html", "awx-alternative.html", "aap-alternative.html",
		"ascender-alternative.html", "semaphore-alternative.html", "rundeck-alternative.html",
		filepath.Join("docs", "comparison.html"),
	}
	for testNum, page := range pages {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("..", "..", "site", page)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			table := tableBlock.FindStringSubmatch(string(body))
			if table == nil {
				t.Fatalf("%s has no table, so there is nothing to open with the frame rows", page)
			}
			rows := tableRow.FindAllStringSubmatch(table[1], -1)
			if len(rows) < len(frameRows)+1 {
				t.Fatalf("%s: the first table has %d rows, too few to carry the frame", page, len(rows))
			}
			for i, want := range frameRows {
				got := rowLabel(rows[i+1][1])
				if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
					t.Errorf("%s: row %d is %q, want one naming %q", page, i+1, got, want)
				}
			}
		})
	}
}
