package audit

import "strings"

// canonicalStrings serializes a list of strings as a compact JSON array using the escaping RFC 8785
// requires, which is what the audit chain profile hashes.
//
// The standard library is deliberately not used here. encoding/json escapes <, >, and & as <,
// >, and & so that output is safe to embed in HTML, and it escapes U+2028 and U+2029 so
// output is safe as JavaScript. Neither escape is part of canonical JSON. A verifier written from
// the format spec produces the unescaped bytes, so any audit path or actor containing one of those
// characters hashed to a value no independent verifier could reproduce, and the chain silently
// failed to verify outside this codebase. A request path reaches the audit log percent-decoded, and
// & is a legal character in a path segment, so this was reachable without any encoding trick.
//
// Escaping here matches RFC 8785: the two mandatory escapes, the five short forms, \u00xx for the
// remaining control characters, and every other character emitted as itself.
func canonicalStrings(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		canonicalString(&b, v)
	}
	b.WriteByte(']')
	return b.String()
}

// canonicalString writes one JSON string with RFC 8785 escaping.
func canonicalString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xF])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
