package server

// maxListRows bounds how many rows one configuration list response carries.
//
// The run list has paged since it shipped, because run history is unbounded by nature. The
// configuration lists, users, tokens, templates, schedules, triggers and organizations, returned
// their whole table with no limit and no pagination, on the assumption that nobody has very many.
// An install that imports from AWX gets hundreds of templates in one pass and thousands of schedules
// from a crontab sweep, and each of those responses is then serialized in full on every page load
// and every poll.
//
// This bounds the response, not the query: the store still reads its table, and giving these stores
// a real limit and offset is a wider change across three implementations. What it buys is that no
// single request can be made arbitrarily large by the size of the install, and that the caller is
// told when it was cut rather than being handed a prefix it will read as the whole set.
const maxListRows = 1000

// cappedList returns at most maxListRows of a list and the full length, so a response can say what
// it left out. It is the same shape cappedHosts gives the fleet views.
func cappedList[T any](all []T) (shown []T, total int) {
	if len(all) <= maxListRows {
		return all, len(all)
	}
	return all[:maxListRows], len(all)
}
