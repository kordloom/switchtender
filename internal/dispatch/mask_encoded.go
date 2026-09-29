package dispatch

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// minEncodedCore is the shortest run of base64 the masker redacts as a secret's encoding. Base64 is
// dense, so a short run turns up by chance in any large encoded blob. A run this long that matches
// carries 48 bits of the secret, which is the secret rather than a coincidence.
const minEncodedCore = 8

// encodedForms returns the other forms a tool commonly prints a secret in: hex in either case, the
// base64 the secret alone determines at each alignment it can take inside a longer encoded blob,
// in the standard and URL alphabets, its URL-escaped forms, and its JSON-escaped forms. Every form
// is derived from the secret and nothing else, so redacting one redacts the secret and no more.
func encodedForms(s string) []string {
	b := []byte(s)
	h := hex.EncodeToString(b)
	forms := []string{h, strings.ToUpper(h)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		for offset := range 3 {
			if core := base64Core(enc, b, offset); len(core) >= minEncodedCore {
				forms = append(forms, core)
			}
		}
	}
	escaped := []string{url.QueryEscape(s), url.PathEscape(s)}
	if utf8.ValidString(s) {
		escaped = append(escaped, jsonEscape(s, false), jsonEscape(s, true))
		if quoted, err := json.Marshal(s); err == nil {
			escaped = append(escaped, string(quoted[1:len(quoted)-1]))
		}
	}
	for _, e := range escaped {
		if e != s {
			forms = append(forms, e)
		}
	}
	return forms
}

// base64Core returns the base64 characters that b alone determines when b starts offset bytes into
// the encoded data. Each character carries six bits, so the characters at either edge of b also
// carry bits of whatever surrounds it. The core is the run between them, the same in every blob
// that holds b at that alignment, whatever comes before or after it.
func base64Core(enc *base64.Encoding, b []byte, offset int) string {
	buf := make([]byte, offset+len(b))
	copy(buf[offset:], b)
	text := enc.WithPadding(base64.NoPadding).EncodeToString(buf)
	// Character k carries bits [6k, 6k+6), and b holds bits [8*offset, 8*(offset+len(b))).
	first := (8*offset + 5) / 6
	last := 8*(offset+len(b))/6 - 1
	if last < first {
		return ""
	}
	return text[first : last+1]
}

// jsonEscape returns s as it appears inside a JSON string: quotes, backslashes, and control
// characters escaped. With asciiOnly, every character past ASCII is a \u escape as well, which is
// how Python writes JSON by default, and so how Ansible's JSON output carries a password with an
// accent in it.
func jsonEscape(s string, asciiOnly bool) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\b':
			sb.WriteString(`\b`)
		case r == '\f':
			sb.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(&sb, `\u%04x`, r)
		case asciiOnly && r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&sb, `\u%04x\u%04x`, hi, lo)
		case asciiOnly && r >= 0x80:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
