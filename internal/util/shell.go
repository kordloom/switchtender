package util

import (
	"regexp"
	"strings"
)

// shellSafe matches an argument a POSIX shell reads as one word with no quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote wraps s in single quotes so a shell reads it as exactly one argument, whatever it
// contains.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellArg returns s as a shell argument, quoted only when it needs to be, so a command printed for
// a person to copy stays readable for the ordinary path and still works for a path with a space.
func ShellArg(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return ShellQuote(s)
}
