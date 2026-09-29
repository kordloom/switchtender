// Package util holds the small cross-package string helpers, one implementation instead of a copy
// per package.
package util

import "unicode/utf8"

// FirstNonEmpty returns the first value that is not empty, or empty when all are.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Clip shortens s to at most limit bytes without splitting a UTF-8 rune, appending an ellipsis when
// the value was cut.
func Clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
