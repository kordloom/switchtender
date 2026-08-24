package util

import (
	"strings"
	"unicode/utf8"
)

// replacement stands in for a byte that cannot be stored as text.
const replacement = "�"

// SafeText returns s with anything a text column cannot hold replaced: a NUL byte, and any byte
// sequence that is not valid UTF-8.
//
// The two storage backends disagreed about these, and the disagreement lost data rather than
// reporting it. SQLite stores arbitrary bytes, so a run whose error text carried a stray byte
// finished normally. PostgreSQL refuses both with SQLSTATE 22021, so the same terminal write failed,
// the run stayed running until the lease sweep interrupted it, and its real outcome and exit code
// were gone. The text arrives from a tool's output, a foreign-locale inventory, or a JSON body
// carrying a legal escaped NUL, so it is somebody else's bytes rather than a fault here.
//
// Replacing is deliberate where refusing would be worse: the point is not to preserve the exact
// bytes a playbook printed, it is to keep the record of what the run did. A visible replacement
// character says something was unrepresentable; a stranded run says nothing at all.
func SafeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, replacement), "\x00", replacement)
}

// SafeTexts returns in with SafeText applied to every element, and the same slice when none changed.
func SafeTexts(in []string) []string {
	for i, s := range in {
		if cleaned := SafeText(s); cleaned != s {
			out := make([]string, len(in))
			copy(out, in)
			out[i] = cleaned
			for j := i + 1; j < len(out); j++ {
				out[j] = SafeText(out[j])
			}
			return out
		}
	}
	return in
}

// SafeStringMap returns m with SafeText applied to every key and value.
func SafeStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[SafeText(k)] = SafeText(v)
	}
	return out
}

// SafeAnyMap returns m with SafeText applied to every key, and to every value that is a string or a
// nested map or slice of them. A value of any other type is carried through untouched, since only
// text is what a text column refuses.
func SafeAnyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[SafeText(k)] = safeAny(v)
	}
	return out
}

// safeAny cleans a value of unknown type, recursing through maps and slices.
func safeAny(v any) any {
	switch t := v.(type) {
	case string:
		return SafeText(t)
	case map[string]any:
		return SafeAnyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = safeAny(e)
		}
		return out
	default:
		return v
	}
}
