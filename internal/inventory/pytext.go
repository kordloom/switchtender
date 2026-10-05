package inventory

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The native inventory engine reads documents the way Ansible's Python reads them, so these helpers
// reproduce the few Python string rules the INI and YAML plugins lean on. Go's own definitions of
// whitespace and line ends differ from Python's at the edges, and an edge is exactly where an
// imitation that is almost right resolves a host Ansible would not.

// pyIsSpace reports whether r is whitespace to Python's str.isspace, which is what str.strip
// removes and what the INI section patterns read as \s: Go's unicode.IsSpace plus the four
// information separators U+001C to U+001F, which Python counts and Go does not.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyIsWord reports whether r is a word character to Python's re module on a str pattern: a letter,
// a number, or an underscore.
func pyIsWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// pyStrip trims Python whitespace from both ends of s, as str.strip does.
func pyStrip(s string) string {
	return strings.TrimFunc(s, pyIsSpace)
}

// pyLineBreak reports whether r ends a line to Python's str.splitlines.
func pyLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// pySplitLines splits s at every line boundary Python's str.splitlines recognizes, dropping the
// boundaries. A carriage return followed by a line feed is one boundary, and a trailing boundary
// does not produce an empty last line.
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !pyLineBreak(r) {
			i += size
			continue
		}
		out = append(out, s[start:i])
		i += size
		if r == '\r' && i < len(s) && s[i] == '\n' {
			i++
		}
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyTruthy reports whether a decoded value is true to Python's bool: everything but None, False, a
// zero number, and an empty string, list, or mapping.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case unsafeString:
		return t != ""
	case pyNumber:
		return !t.zero()
	case []any:
		return len(t) > 0
	case *orderedMap:
		return t.Len() > 0
	}
	return true
}
