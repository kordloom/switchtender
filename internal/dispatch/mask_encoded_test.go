package dispatch

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// recoverable reports whether six consecutive bytes of secret, or the whole of a shorter one, can be
// read back out of text by undoing an encoding a tool might have applied: the text as it stands,
// every hex run decoded at either nibble alignment, every base64 run decoded at every character
// alignment in both alphabets, and the text URL-unescaped or JSON-unescaped. It asks the attacker's
// question rather than the masker's, so it holds the masker to the leak and not to its own design.
func recoverable(text, secret string) bool {
	// Encodings a tool spreads over lines or spaces, base64 wrapped at a column and a byte dump with
	// a space between bytes, decode only once the whitespace between their pieces is gone.
	if stripped := strings.Join(strings.Fields(text), ""); stripped != text && recoverableAsIs(stripped, secret) {
		return true
	}
	return recoverableAsIs(text, secret)
}

// recoverableAsIs reports whether any six-byte window of secret reads back out of text by any
// decoding recoverable tries, without joining pieces across whitespace.
func recoverableAsIs(text, secret string) bool {
	n := min(6, len(secret))
	var windows []string
	for i := 0; i+n <= len(secret); i++ {
		windows = append(windows, secret[i:i+n])
	}
	candidates := []string{text, jsonUnescape(text)}
	if u, err := url.QueryUnescape(text); err == nil {
		candidates = append(candidates, u)
	}
	if u, err := url.PathUnescape(text); err == nil {
		candidates = append(candidates, u)
	}
	isHex := func(r rune) bool { return strings.ContainsRune("0123456789abcdefABCDEF", r) }
	for _, run := range strings.FieldsFunc(text, func(r rune) bool { return !isHex(r) }) {
		for off := range 2 {
			if s := run[min(off, len(run)):]; len(s) >= 2 {
				if b, err := hex.DecodeString(s[:len(s)/2*2]); err == nil {
					candidates = append(candidates, string(b))
				}
			}
		}
	}
	for _, alpha := range []struct {
		Extra string
		Enc   *base64.Encoding
	}{{"+/", base64.RawStdEncoding}, {"-_", base64.RawURLEncoding}} {
		inAlpha := func(r rune) bool {
			return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
				strings.ContainsRune(alpha.Extra, r)
		}
		for _, run := range strings.FieldsFunc(text, func(r rune) bool { return !inAlpha(r) }) {
			for off := range 4 {
				if s := run[min(off, len(run)):]; len(s) >= 4 {
					if b, err := alpha.Enc.DecodeString(s[:len(s)/4*4]); err == nil {
						candidates = append(candidates, string(b))
					}
				}
			}
		}
	}
	for _, c := range candidates {
		for _, w := range windows {
			if strings.Contains(c, w) {
				return true
			}
		}
	}
	return false
}

// jsonUnescape undoes JSON string escapes wherever they appear in s, leaving everything else as it
// is, so a secret printed inside a JSON document reads back as itself.
func jsonUnescape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case '"', '\\', '/':
			sb.WriteByte(c)
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'u':
			if i+4 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					r := rune(v)
					i += 4
					if utf16.IsSurrogate(r) && i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
						if lo, err := strconv.ParseUint(s[i+3:i+7], 16, 32); err == nil {
							r = utf16.DecodeRune(r, rune(lo))
							i += 6
						}
					}
					sb.WriteRune(r)
					continue
				}
			}
			sb.WriteByte('\\')
			sb.WriteByte('u')
		default:
			sb.WriteByte('\\')
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// TestMaskerRedactsEncodedSecrets pins that a secret printed in a form a tool commonly re-encodes it
// in cannot be read back out of the log. Literal masking alone left every one of these rows
// recoverable: base64 on its own or inside a basic auth header at each of the three byte alignments,
// either base64 alphabet, hex in either case, URL escaping, and the JSON escaping Python and Go each
// write. The encoded outputs come from the standard library or are written out by hand, never from
// the masker's own encoder.
//
//nolint:funlen // Test function.
func TestMaskerRedactsEncodedSecrets(t *testing.T) {
	t.Parallel()
	const (
		plain    = "hunter2-sw0rdf1sh"
		symbols  = "~~~>>>???key"
		special  = "p@ss w/rd&x=1"
		quoted   = `tok"en\back-99`
		accented = "p\xc3\xa4ssw\xc3\xb6rd-\xc3\xa4\xc3\xb6\xc3\xbc"
		htmlish  = "a<b>c&d-token"
	)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	b64url := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	// u writes the JSON escape for one character by hand, independent of the masker's escaper.
	u := func(r rune) string { return fmt.Sprintf(`\u%04x`, r) }
	tests := []struct {
		Name   string
		Secret string
		Output string
	}{{ // Test 0: Encoded on its own, as echo -n "$SECRET" | base64 prints it.
		Name: "base64 alone", Secret: plain, Output: "token=" + b64(plain) + "\n",
	}, { // Test 1: Encoded with the newline echo adds, which changes the last characters.
		Name: "base64 of echo", Secret: plain, Output: "token=" + b64(plain+"\n") + "\n",
	}, { // Test 2: Inside a basic auth header, starting one byte into a group.
		Name: "basic auth offset one", Secret: plain, Output: "Authorization: Basic " + b64("bob:"+plain),
	}, { // Test 3: Inside a basic auth header, starting two bytes into a group.
		Name: "basic auth offset two", Secret: plain, Output: "Authorization: Basic " + b64("user:"+plain),
	}, { // Test 4: Inside a basic auth header, starting on a group boundary.
		Name: "basic auth offset zero", Secret: plain, Output: "Authorization: Basic " + b64("admin:"+plain),
	}, { // Test 5: The standard alphabet, where this secret encodes with + and /.
		Name: "standard alphabet", Secret: symbols, Output: "cfg: " + b64(symbols),
	}, { // Test 6: The URL alphabet, where the same secret encodes with - and _.
		Name: "url alphabet", Secret: symbols, Output: "cfg: " + b64url(symbols),
	}, { // Test 7: Lowercase hex, as xxd -p and most hex dumps print it.
		Name: "hex lower", Secret: plain, Output: "dump " + hex.EncodeToString([]byte(plain)),
	}, { // Test 8: Uppercase hex.
		Name: "hex upper", Secret: plain, Output: "DUMP " + strings.ToUpper(hex.EncodeToString([]byte(plain))),
	}, { // Test 9: Query escaped inside a URL a tool logs.
		Name: "query escaped", Secret: special, Output: "GET /login?pw=p%40ss+w%2Frd%26x%3D1 200",
	}, { // Test 10: Inside a JSON document, with its quote and backslash escaped.
		Name: "json escaped", Secret: quoted, Output: `{"password": "tok\"en\\back-99"}`,
	}, { // Test 11: Inside JSON Python wrote, every accented letter a \u escape.
		Name: "python json", Secret: accented,
		Output: `{"pw": "p` + u(0xe4) + `ssw` + u(0xf6) + `rd-` + u(0xe4) + u(0xf6) + u(0xfc) + `"}`,
	}, { // Test 12: Inside JSON Go wrote, with its HTML characters escaped.
		Name: "go json", Secret: htmlish,
		Output: `{"pw":"a` + u(0x3c) + `b` + u(0x3e) + `c` + u(0x26) + `d-token"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if strings.Contains(test.Output, test.Secret) {
				t.Fatalf("the output carries the secret as plain text, so this row tests literal "+
					"masking rather than an encoding: %q", test.Output)
			}
			if !recoverable(test.Output, test.Secret) {
				t.Fatalf("the unmasked output does not hold the secret, so this row proves nothing: %q",
					test.Output)
			}
			m := &masker{}
			m.set([]string{test.Secret})
			got := m.redactString(test.Output)
			if recoverable(got, test.Secret) {
				t.Errorf("the secret can be read back out of the masked output %q", got)
			}
			if !strings.Contains(got, maskToken) {
				t.Errorf("nothing was masked in %q", got)
			}
		})
	}
}

// TestEncodedMaskingLeavesUnrelatedOutputAlone pins the other side: encoded output that does not
// hold a secret passes through byte for byte. Masking that fired on any base64 or hex would black
// out the diffs, digests, and certificates runs print all day.
func TestEncodedMaskingLeavesUnrelatedOutputAlone(t *testing.T) {
	t.Parallel()
	m := &masker{}
	m.set([]string{"hunter2-sw0rdf1sh", "~~~>>>???key", `tok"en\back-99`})
	for testNum, output := range []string{
		// Test 0: Base64 of ordinary text.
		"data: " + base64.StdEncoding.EncodeToString([]byte("the quick brown fox jumps over the lazy dog")),
		// Test 1: A digest in hex.
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		// Test 2: An escaped URL and a JSON document that hold no secret.
		`GET /search?q=a%20b%26c 200 {"msg": "he said \"hi\""}`,
	} {
		if got := m.redactString(output); got != output {
			t.Errorf("test %d: unrelated output changed from %q to %q", testNum, output, got)
		}
	}
}

// TestEncodedSecretMaskedAcrossChunks pins that an encoded form split across the writes a pipe
// delivers is masked like a literal one, one byte per write being the worst split there is.
func TestEncodedSecretMaskedAcrossChunks(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-sw0rdf1sh"
	stream := "before " + base64.StdEncoding.EncodeToString([]byte(secret)) + " after"
	m := &masker{}
	m.set([]string{secret})
	sm := &streamMasker{mask: m}
	var got strings.Builder
	for i := range len(stream) {
		got.Write(sm.next([]byte{stream[i]}))
	}
	got.Write(sm.flush())
	if recoverable(got.String(), secret) {
		t.Errorf("the secret can be read back out of the masked stream %q", got.String())
	}
}

// TestBase64CoreHoldsWhateverSurroundsIt pins the property the base64 forms rest on: the core
// computed for an alignment appears in the encoding of every buffer that holds the secret at that
// alignment, whatever bytes come before or after it. The standard library's encoder is the oracle.
func TestBase64CoreHoldsWhateverSurroundsIt(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		return b
	}
	for length := 1; length <= 24; length++ {
		secret := random(length)
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
			for offset := range 3 {
				core := base64Core(enc, secret, offset)
				for trial := range 20 {
					prefix := random(offset + 3*(trial%3))
					suffix := random(trial % 5)
					whole := enc.EncodeToString(append(append(prefix, secret...), suffix...))
					if core != "" && !strings.Contains(whole, core) {
						t.Fatalf("length %d offset %d: core %q is not in %q", length, offset, core, whole)
					}
				}
			}
		}
	}
}

// TestMaskerRedactsWhatToolsActuallyPrint covers the encodings the pre-launch sweep read secrets back
// out of. Every output below was produced by the tool the row names, run on the secret alone, and
// pasted in, rather than rebuilt by this package: a row built with the masker's own encoder proves
// only that the masker agrees with itself. The long secrets wrap, which is what made them leak: each
// line carried a piece of the secret and no line carried the whole.
//
//nolint:funlen // Test function.
func TestMaskerRedactsWhatToolsActuallyPrint(t *testing.T) {
	t.Parallel()
	const (
		ssh = "FAKEssh/M4t+q=\u00e9&\"z"
		aws = "FAKEaws/Sk3+yq9Pz0wXvT7uLrN2mQ8kJ5hG4fD1"
		tok = "FAKEtokabcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvw"
	)
	tests := []struct {
		Name   string
		Secret string
		Output string
	}{{ // Test 0: Ansible urlencode, which keeps a slash.
		Name: "Ansible urlencode, which keeps a slash", Secret: ssh,
		Output: "FAKEssh/M4t%2Bq%3D%C3%A9%26%22z",
	}, { // Test 1: Lowercase percent escapes, as .NET and printf write them.
		Name: "lowercase percent escapes, as .NET and printf write them", Secret: aws,
		Output: "FAKEaws%2fSk3%2byq9Pz0wXvT7uLrN2mQ8kJ5hG4fD1",
	}, { // Test 2: JSON with every slash escaped, as PHP writes it.
		Name: "JSON with every slash escaped, as PHP writes it", Secret: ssh,
		Output: "FAKEssh\\/M4t+q=\\u00e9&\\\"z",
	}, { // Test 3: Base64 wrapped at 76, as Python encodebytes writes it.
		Name: "base64 wrapped at 76, as Python encodebytes writes it", Secret: tok,
		Output: "RkFLRXRva2FiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1u\nb3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3\n",
	}, { // Test 4: Base64 wrapped at 76, as GNU base64 writes it.
		Name: "base64 wrapped at 76, as GNU base64 writes it", Secret: tok,
		Output: "RkFLRXRva2FiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1u\nb3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3\n",
	}, { // Test 5: Hex wrapped at 60, as xxd -p writes it.
		Name: "hex wrapped at 60, as xxd -p writes it", Secret: aws,
		Output: "46414b456177732f536b332b797139507a307758765437754c724e326d51\n386b4a35684734664431\n",
	}, { // Test 6: A byte dump, as GNU od -An -tx1 writes it.
		Name: "a byte dump, as GNU od -An -tx1 writes it", Secret: aws,
		Output: " 46 41 4b 45 61 77 73 2f 53 6b 33 2b 79 71 39 50\n 7a 30 77 58 76 54 37 75 4c 72 4e 32 6d 51 38 6b\n 4a 35 68 47 34 66 44 31\n",
	}, { // Test 7: A byte dump, as BSD od -An -tx1 writes it.
		Name: "a byte dump, as BSD od -An -tx1 writes it", Secret: aws,
		Output: "           46  41  4b  45  61  77  73  2f  53  6b  33  2b  79  71  39  50\n           7a  30  77  58  76  54  37  75  4c  72  4e  32  6d  51  38  6b\n           4a  35  68  47  34  66  44  31                                \n\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if strings.Contains(test.Output, test.Secret) {
				t.Fatalf("the output carries the secret as plain text: %q", test.Output)
			}
			if !recoverable(test.Output, test.Secret) {
				t.Fatalf("the unmasked output does not hold the secret, so this row proves nothing: %q",
					test.Output)
			}
			m := &masker{}
			m.set([]string{test.Secret})
			got := m.redactString(test.Output)
			if recoverable(got, test.Secret) {
				t.Errorf("the secret can be read back out of the masked output %q", got)
			}
			if !strings.Contains(got, maskToken) {
				t.Errorf("nothing was masked in %q", got)
			}
		})
	}
}
