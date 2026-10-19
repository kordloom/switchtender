package server

import "net/http"

// maxListRows bounds how many rows one list response carries.
//
// The run list has paged since it shipped, because run history is unbounded by nature. The
// configuration lists, users, tokens, templates, schedules, triggers, notification targets,
// organizations and organization members, page the same way through pageList: one response carries
// at most this many rows, and the caller reads the rest with offset. An install that imports from
// AWX gets hundreds of templates in one pass and thousands of schedules from a crontab sweep, so no
// single response can be made arbitrarily large by the size of the install.
//
// This bounds the response, not the query: the store still reads its table, and giving these stores
// a real limit and offset, with authorization in SQL, is a wider change across three
// implementations. The fleet, estate and change views cut at the same number with cappedList and
// say "Showing N of M".
const maxListRows = 1000

// cappedList returns at most maxListRows of a list and the full length, so a response can say what
// it left out. It is the same shape cappedHosts gives the fleet views.
func cappedList[T any](all []T) (shown []T, total int) {
	if len(all) <= maxListRows {
		return all, len(all)
	}
	return all[:maxListRows], len(all)
}

// pageList returns the page of a configuration list that the request asks for, with the paging
// fields its response carries.
//
// all is the list the handler read and already filtered to what the caller may read, so total and
// every offset count only readable rows, and a restricted reader's page two never repeats or skips
// a row of page one. Each id parameter narrows the list to the named objects before paging, and
// idOf names a row's object. limit defaults to maxListRows and is never larger. offset reads
// through queryInt, so a negative or non-numeric value is zero, as the run list's is. The order is
// the store's own List order, created_at then id, a total order in every backend.
func pageList[T any](r *http.Request, all []T, idOf func(T) string) (shown []T, total int,
	hasMore bool, nextOffset int) {
	if ids := r.URL.Query()["id"]; len(ids) > 0 {
		want := make(map[string]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
		named := make([]T, 0, len(ids))
		for _, row := range all {
			if want[idOf(row)] {
				named = append(named, row)
			}
		}
		all = named
	}
	limit := queryInt(r, "limit")
	if limit == 0 || limit > maxListRows {
		limit = maxListRows
	}
	offset := min(queryInt(r, "offset"), len(all))
	end := min(offset+limit, len(all))
	return all[offset:end], len(all), end < len(all), end
}
