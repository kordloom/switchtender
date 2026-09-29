package server

import (
	"testing"
)

// TestConfigurationListsAreBounded covers responses whose size was the size of the install.
//
// The run list has paged since it shipped, because run history is unbounded by nature. The
// configuration lists, users, tokens, templates, schedules, triggers and organizations, returned
// their whole table with no limit and no pagination, on the assumption that nobody has very many. An
// install that imports from AWX gets hundreds of templates in one pass, and a crontab sweep produces
// a schedule per line; each of those responses was then serialized in full on every page load and
// every poll.
//
// This bounds the response rather than the query. The store still reads its table, and giving these
// stores a real limit and offset is a wider change across three implementations, so the honest
// statement of what this buys is: no single request can be made arbitrarily large by the size of the
// install, and a caller handed a prefix is told that is what it is.
func TestConfigurationListsAreBounded(t *testing.T) {
	t.Parallel()

	big := make([]int, maxListRows*2+7)
	shown, total := cappedList(big)
	if len(shown) != maxListRows {
		t.Errorf("shown = %d rows, want the cap of %d", len(shown), maxListRows)
	}
	if total != len(big) {
		t.Errorf("total = %d, want %d: a caller shown a prefix has to be told of what",
			total, len(big))
	}

	// Every ordinary install is under the cap, where Count and Total agree and nothing is cut.
	small := make([]int, 12)
	shown, total = cappedList(small)
	if len(shown) != 12 || total != 12 {
		t.Errorf("small install: shown %d total %d, want 12 and 12", len(shown), total)
	}

	// Exactly at the cap is not truncated, so no install is told it is missing rows it is not.
	exact := make([]int, maxListRows)
	shown, total = cappedList(exact)
	if len(shown) != maxListRows || total != maxListRows {
		t.Errorf("at the cap: shown %d total %d, want %d twice", len(shown), total, maxListRows)
	}

	// An empty list stays empty rather than becoming nil-with-a-count.
	shown, total = cappedList([]int{})
	if len(shown) != 0 || total != 0 {
		t.Errorf("empty list: shown %d total %d, want 0 and 0", len(shown), total)
	}
}
