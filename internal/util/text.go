package util

import (
	"strings"
	"unicode"
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
	if IsSafeText(s) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, replacement), "\x00", replacement)
}

// IsSafeText reports whether every store keeps s as text unchanged: it is valid UTF-8 and holds no
// NUL byte, the two things PostgreSQL refuses in a text value with SQLSTATE 22021.
func IsSafeText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
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

// JoinWords joins words the way a sentence lists them, with conjunction before the last: "a",
// "a and b", "a, b, and c". A list interpolated into a sentence reads as prose rather than as a
// slice printed into the middle of one.
func JoinWords(words []string, conjunction string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " " + conjunction + " " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", " + conjunction + " " + words[len(words)-1]
}

// HiddenRuneName returns the Unicode name of r when r is a character that changes how the text
// around it is displayed, or what a terminal does with it, without being visible itself, and empty
// for any other character. The characters are the bidirectional embedding, override, isolate, and
// mark characters, the zero-width space, joiners, word joiner, and byte order mark, and every
// control character except a tab and a line feed.
//
// This is the one list of such characters. A decision reason has them removed, since a reason is
// never refused for what it looks like, and a run's command, playbook, labels, and intent and a
// template or inventory name are refused for holding one, since an approver reads those to decide
// what they are allowing.
func HiddenRuneName(r rune) string {
	switch r {
	case '\t', '\n':
		return ""
	case 0x00:
		return "NULL"
	case 0x08:
		return "BACKSPACE"
	case 0x0D:
		return "CARRIAGE RETURN"
	case 0x1B:
		return "ESCAPE"
	case 0x7F:
		return "DELETE"
	case 0x061C:
		return "ARABIC LETTER MARK"
	case 0x200B:
		return "ZERO WIDTH SPACE"
	case 0x200C:
		return "ZERO WIDTH NON-JOINER"
	case 0x200D:
		return "ZERO WIDTH JOINER"
	case 0x200E:
		return "LEFT-TO-RIGHT MARK"
	case 0x200F:
		return "RIGHT-TO-LEFT MARK"
	case 0x202A:
		return "LEFT-TO-RIGHT EMBEDDING"
	case 0x202B:
		return "RIGHT-TO-LEFT EMBEDDING"
	case 0x202C:
		return "POP DIRECTIONAL FORMATTING"
	case 0x202D:
		return "LEFT-TO-RIGHT OVERRIDE"
	case 0x202E:
		return "RIGHT-TO-LEFT OVERRIDE"
	case 0x2060:
		return "WORD JOINER"
	case 0x2066:
		return "LEFT-TO-RIGHT ISOLATE"
	case 0x2067:
		return "RIGHT-TO-LEFT ISOLATE"
	case 0x2068:
		return "FIRST STRONG ISOLATE"
	case 0x2069:
		return "POP DIRECTIONAL ISOLATE"
	case 0xFEFF:
		return "ZERO WIDTH NO-BREAK SPACE"
	}
	if unicode.IsControl(r) {
		return "CONTROL CHARACTER"
	}
	return ""
}
