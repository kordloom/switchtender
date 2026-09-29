package util

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PyQuote renders s as a single-quoted Python string literal, the form Ansible's INI inventory reads
// back as exactly s. Ansible passes an INI value through Python's literal parser, so an unquoted 1.10
// becomes a float and an unquoted True a boolean. Quoted, every string stays the string it was.
func PyQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			// Control characters and the separators Python counts as line breaks are escaped, so the
			// literal stays on the one line an INI inventory gives it.
			switch {
			case r == utf8.RuneError:
				b.WriteString(`\ufffd`)
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
				fmt.Fprintf(&b, `\x%02x`, r)
			case r == 0x2028 || r == 0x2029:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// PyUnquote decodes a single- or double-quoted Python string literal, reporting false for anything
// else. It reads the escapes PyQuote writes and the other common ones, and keeps an unrecognized
// escape's backslash as Python does.
func PyUnquote(s string) (string, bool) {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') || s[len(s)-1] != s[0] {
		return "", false
	}
	quote, body := s[0], s[1:len(s)-1]
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == quote {
			// An unescaped quote inside means this was not one literal.
			return "", false
		}
		if c != '\\' || i+1 == len(body) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := body[i]; e {
		case '\\', '\'', '"':
			b.WriteByte(e)
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case 'x', 'u', 'U':
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if i+1+width > len(body) {
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			}
			code, err := strconv.ParseUint(body[i+1:i+1+width], 16, 32)
			if err != nil {
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			}
			b.WriteRune(rune(code))
			i += width
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String(), true
}
