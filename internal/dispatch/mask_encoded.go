package dispatch

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
	// Python's quote and Ansible's urlencode filter keep a slash, so a secret holding one printed
	// through that filter matched neither of Go's escaped forms.
	escaped := []string{url.QueryEscape(s), url.PathEscape(s), percentEncode(b, "/")}
	// Lowercase escape digits are what .NET's UrlEncode and a hand-rolled printf %%%02x write.
	for _, e := range escaped[:3] {
		if lower := lowerEscapes(e); lower != e {
			escaped = append(escaped, lower)
		}
	}
	if utf8.ValidString(s) {
		jsonForms := []string{jsonEscape(s, false), jsonEscape(s, true)}
		if quoted, err := json.Marshal(s); err == nil {
			jsonForms = append(jsonForms, string(quoted[1:len(quoted)-1]))
		}
		for _, j := range jsonForms {
			escaped = append(escaped, j)
			// PHP's json_encode escapes every slash by default.
			if strings.Contains(j, "/") {
				escaped = append(escaped, strings.ReplaceAll(j, "/", `\/`))
			}
		}
	}
	for _, e := range escaped {
		if e != s {
			forms = append(forms, e)
		}
	}
	return append(forms, wrappedForms(b, h)...)
}

// wrappedForms returns the forms a tool prints across several lines when the secret is long enough:
// base64 wrapped at 76 columns as GNU base64 and Python's encodebytes write it and at 64 as openssl
// writes it, hex wrapped at 60 columns as xxd -p writes it, and the byte dump od -An -tx1 writes in
// its GNU and BSD layouts. Each matches a secret encoded on its own, which is how a script encodes
// one, and each begins at the first character the secret determines, so the indentation a tool puts
// in front of a line does not stop it matching.
func wrappedForms(b []byte, h string) []string {
	var forms []string
	b64 := base64.StdEncoding.EncodeToString(b)
	for _, width := range []int{76, 64} {
		if w := wrapped(b64, width); w != "" {
			forms = append(forms, w)
		}
	}
	if w := wrapped(h, 60); w != "" {
		forms = append(forms, w)
	}
	return append(forms, odDump(b, " ", "\n "), odDump(b, "  ", "\n           "))
}

// wrapped returns text broken into lines of width, or the empty string when it fits on one line and
// so has no wrapped form.
func wrapped(text string, width int) string {
	if len(text) <= width {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < len(text); i += width {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(text[i:min(i+width, len(text))])
	}
	return sb.String()
}

// odDump returns b as od -An -tx1 prints it, sixteen bytes to a line, starting at the first byte's
// digits: sep goes between two bytes on a line and newline between the last byte of one line and the
// first of the next. GNU od separates bytes with one space, and BSD od with two and an indent.
func odDump(b []byte, sep, newline string) string {
	var sb strings.Builder
	for i, c := range b {
		switch {
		case i == 0:
		case i%16 == 0:
			sb.WriteString(newline)
		default:
			sb.WriteString(sep)
		}
		fmt.Fprintf(&sb, "%02x", c)
	}
	return sb.String()
}

// percentEncode returns b percent-encoded as Python's quote does: the unreserved characters and
// those in safe are kept, every other byte becomes an uppercase escape.
func percentEncode(b []byte, safe string) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', strings.IndexByte(safe, c) >= 0:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// lowerEscapes returns s with the hex digits of every percent escape in lowercase.
func lowerEscapes(s string) string {
	return percentEscape.ReplaceAllStringFunc(s, strings.ToLower)
}

// percentEscape matches one percent escape with uppercase hex digits.
var percentEscape = regexp.MustCompile(`%[0-9A-F]{2}`)

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
