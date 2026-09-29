package audit

import (
	"strings"
	"unicode/utf8"
)

// escapeInvalidUTF8 returns s with every byte that is not part of a well-formed UTF-8 sequence
// replaced by an uppercase percent escape, and every literal percent rewritten as %25 so the result
// reads back unambiguously.
//
// The canonical form hashes text, not bytes: ranging over a string decodes it, so every invalid byte
// arrives as U+FFFD and two different byte strings hash to the same chain link. A request path is
// the reachable case, because net/http leaves raw bytes in URL.Path, which means DELETE
// /v1/credentials/\xff and DELETE /v1/credentials/\xfe were indistinguishable in the audit record.
// That is a hole in the tamper evidence the chain exists to provide.
//
// It cannot be fixed at hash time. The reference verifier reads a JSON bundle that has already been
// decoded as UTF-8, so a raw invalid byte has no representation in the form it can reproduce. The
// entry has to become valid UTF-8 before anything commits to it, which is what this does.
//
// Percent is escaped as well because otherwise the escaping reintroduces the same collision one
// level up: a path holding the three characters %FF and a path holding the single byte 0xFF would
// both come out as %FF, and both are reachable from a request.
func escapeInvalidUTF8(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, '%') {
		return s
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' {
			b.WriteString("%25")
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('%')
			b.WriteByte(hex[s[i]>>4])
			b.WriteByte(hex[s[i]&0xF])
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
