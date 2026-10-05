package util

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// wordEnds marks the bytes that end an unquoted shell word: the whitespace the assignment patterns
// read as a space, and the separators that end a command.
var wordEnds = func() (t [256]bool) {
	for _, c := range []byte(" \t\n\f\r" + shellSeparators) {
		t[c] = true
	}
	return t
}()

// shellWord reads the shell word at the start of s and returns how many bytes it spans, writing the
// value a shell gives it to value when value is not nil.
//
// A value written on a command line is rarely one quoted run. The shell joins every quoted run,
// escape, and bare character up to the first unquoted space into one word, so each of these assigns
// the value on its right, the second being the idiom for a quote inside a single-quoted string:
//
//	password=''x          x
//	password='pa'\''ss'   pa'ss
//	password=x\ y         x y
//
// Reading only the first quoted run masked the quotes and left the rest of the password in the text,
// and handed the masker a fragment, or for the first an empty string, in place of the password.
//
// closer is the quote a line opened before the word, or zero. Inside it the text belongs to that
// quoted string, so the closer ends the word and a backslash escapes only what the shell lets it
// escape there, while quotes of the other kind still group text for the program the string is handed
// to, as the single quotes in sh -c "PASS='a b' deploy" do.
//
// A quote that never closes is read as an ordinary byte, so the rest of the word is still taken
// rather than left in the clear.
func shellWord(s string, closer byte, value *strings.Builder) int {
	emit := func(t string) {
		if value != nil {
			value.WriteString(t)
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case wordEnds[c] || (closer != 0 && c == closer):
			return i
		case c == '\\':
			i += shellEscape(s[i:], closer, value)
		case c == '$' && closer == 0 && i+1 < len(s) && s[i+1] == '\'':
			end := quotedEnd(s, i+1, closer, true)
			if end < 0 {
				emit("$")
				i++
				continue
			}
			emit(ansiC(s[i+2 : end-1]))
			i = end
		case c == '\'' || c == '"':
			end := quotedEnd(s, i, closer, c == '"' || closer == '"')
			if end < 0 {
				emit(s[i : i+1])
				i++
				continue
			}
			if c == '"' {
				emit(doubleQuoted(s[i+1 : end-1]))
			} else {
				emit(s[i+1 : end-1])
			}
			i = end
		default:
			j := i + 1
			for j < len(s) && !wordEnds[s[j]] && s[j] != closer && s[j] != '\\' && s[j] != '$' &&
				s[j] != '\'' && s[j] != '"' {
				j++
			}
			emit(s[i:j])
			i = j
		}
	}
	return len(s)
}

// shellWordLen is shellWord as an assignment form's reader: how far a name=value value runs.
func shellWordLen(rest, _ string, closer byte) (int, bool) {
	return shellWord(rest, closer, nil), false
}

// shellValue returns the value a shell gives the word raw, read inside closer.
func shellValue(raw string, closer byte) string {
	var b strings.Builder
	shellWord(raw, closer, &b)
	return b.String()
}

// shellEscape reads the backslash at the start of s, writing what it stands for to value, and
// returns how many bytes it covers.
//
// Unquoted, a backslash makes the next byte ordinary, and before a newline it joins two lines. Inside
// a double-quoted string it escapes only the characters the shell gives meaning there, and an escaped
// quote pair groups text for the program the string is handed to, as in
// ssh host "export PASS=\"a b\"". Inside a single-quoted string the shell leaves it alone, and reading
// it as an escape there only lengthens the word, which is the safe direction.
func shellEscape(s string, closer byte, value *strings.Builder) int {
	emit := func(t string) {
		if value != nil {
			value.WriteString(t)
		}
	}
	if len(s) == 1 {
		emit(s)
		return 1
	}
	next := s[1]
	switch closer {
	case 0:
		if next != '\n' {
			emit(s[1:2])
		}
	case '\'':
		emit(s[:2])
	default:
		if next == '"' {
			if end := escapedQuoteEnd(s); end > 0 {
				emit(doubleQuoted(s[2 : end-2]))
				return end
			}
		}
		switch next {
		case '"', '\\', '$', '`':
			emit(s[1:2])
		case '\n':
		default:
			emit(s[:2])
		}
	}
	return 2
}

// escapedQuoteEnd returns the length of the escaped-quote run \"...\" at the start of s, or zero when
// a bare quote, which closes the string holding it, comes first.
func escapedQuoteEnd(s string) int {
	for i := 2; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) && s[i+1] == '"' {
				return i + 2
			}
			i++
		case '"':
			return 0
		}
	}
	return 0
}

// quotedEnd returns the index just past the quote that closes the run s[open] begins, or -1 when it
// does not close before the end of s or before closer, the quote holding the whole word. escapes is
// whether a backslash protects the byte after it, which is true in a double-quoted run and false in a
// single-quoted one, where the shell reads it as an ordinary character.
func quotedEnd(s string, open int, closer byte, escapes bool) int {
	q := s[open]
	for i := open + 1; i < len(s); i++ {
		switch c := s[i]; {
		case escapes && c == '\\':
			i++
		case c == q:
			return i + 1
		case closer != 0 && c == closer:
			return -1
		}
	}
	return -1
}

// doubleQuoted returns the value of the inside of a double-quoted shell string, where a backslash
// escapes only a quote, a backslash, a dollar sign, a backtick, or a newline.
func doubleQuoted(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '$', '`':
				b.WriteByte(s[i+1])
				i++
				continue
			case '\n':
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ansiC returns the value of the inside of a $'...' string, decoding the escapes bash reads there. A
// password holding a character a script cannot type plainly is written this way, and the value the
// program prints is the decoded one.
func ansiC(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch e := s[i]; e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '\'', '"', '?':
			b.WriteByte(e)
		case 'c':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i] & 0x1f)
			} else {
				b.WriteString(`\c`)
			}
		case 'x', 'u', 'U':
			n := hexRun(s[i+1:], hexWidth(e))
			if n == 0 {
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			}
			v, _ := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if e == 'x' {
				b.WriteByte(byte(v))
			} else if utf8.ValidRune(rune(v)) {
				b.WriteRune(rune(v))
			}
			i += n
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n := 1
			for n < 3 && i+n < len(s) && s[i+n] >= '0' && s[i+n] <= '7' {
				n++
			}
			v, _ := strconv.ParseUint(s[i:i+n], 8, 16)
			b.WriteByte(byte(v))
			i += n - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String()
}

// hexWidth returns how many hex digits the escape named by e reads at most.
func hexWidth(e byte) int {
	switch e {
	case 'x':
		return 2
	case 'u':
		return 4
	}
	return 8
}

// hexRun returns how many of the first width bytes of s are hex digits, stopping at the first that
// is not.
func hexRun(s string, width int) int {
	n := 0
	for n < width && n < len(s) && strings.IndexByte("0123456789abcdefABCDEF", s[n]) >= 0 {
		n++
	}
	return n
}

// openQuote returns the quote a shell is still inside at the end of line, or zero. It is how a value
// learns that a quote opened before its name holds it, as in psql "host=db password=hunter2", where
// the closing quote belongs to the string and not to the password.
func openQuote(line string) byte {
	var q byte
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && q != '\'':
			i++
		case q == 0 && (c == '\'' || c == '"'):
			q = c
		case c == q:
			q = 0
		}
	}
	return q
}

// yamlValue reads the value of a name: value assignment starting at rest and returns how many bytes
// it spans and whether its quotes stay around the mask. line is the text before the value on its own
// line and closer the quote that line left open, if any.
//
// A quoted scalar ends at its closing quote only where YAML or JSON would end it: at the end of the
// line, at the quote holding the whole text, at a separator, or at the comma or bracket that follows
// a value in a flow mapping such as a JSON body. Anything else joined onto it means the text was not
// a scalar at all, and the value runs on the way an unquoted one does. Stopping at the quote there
// left the rest in the clear, and a second pass over the redacted text then masked what the first
// had kept, so redacting twice gave a different answer than redacting once.
//
// In a flow mapping the quotes stay around the mask, so {"password":"x","user":"y"} redacts to valid
// JSON, and redacting it again reads the same quoted scalar rather than an unquoted value running
// to the end of the line.
//
// A block scalar, password: | followed by indented lines, takes those lines. Reading only the
// indicator masked a | and left a private key on the lines below it.
func yamlValue(rest, line string, closer byte) (n int, keepQuotes bool) {
	if n := yamlBlockLen(rest, line); n > 0 {
		return n, false
	}
	start := 0
	if c := rest[0]; c == '"' || c == '\'' {
		end := yamlQuotedEnd(rest, closer)
		if end < 0 {
			return plainLen(rest, closer, false), false
		}
		if end < len(rest) && strings.IndexByte(",}]", rest[end]) >= 0 {
			return end, true
		}
		start = end
	}
	return start + plainLen(rest[start:], closer, true), false
}

// yamlQuotedEnd returns the index just past the closing quote of the YAML scalar rest begins with,
// or -1 when it does not close before the end of the text or closer. A single-quoted scalar escapes
// its quote by doubling it and a double-quoted one with a backslash.
func yamlQuotedEnd(rest string, closer byte) int {
	q := rest[0]
	for i := 1; i < len(rest); i++ {
		switch c := rest[i]; {
		case q == '"' && c == '\\':
			i++
		case c == q:
			if q == '\'' && i+1 < len(rest) && rest[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		case closer != 0 && c == closer && c != q:
			return -1
		}
	}
	return -1
}

// plainLen returns how much of s an unquoted value takes: the rest of the line, cut at the quote
// holding the whole text and, when separators is true, at a command separator, with trailing
// whitespace left out. A backslash protects the byte after it from being read as the closer, which
// is how a quote is written inside the string that closer ends.
func plainLen(s string, closer byte, separators bool) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && closer == '"' {
			i++
			continue
		}
		if c == '\n' || c == '\r' || (closer != 0 && c == closer) ||
			(separators && strings.IndexByte(shellSeparators, c) >= 0) {
			n = i
			break
		}
	}
	for n > 0 && (s[n-1] == ' ' || s[n-1] == '\t') {
		n--
	}
	return n
}

// yamlBlockLen returns how many bytes of rest a block scalar takes when rest begins with one: the
// indicator line and every following line indented deeper than the name, with blank lines between
// them. It returns zero when rest is not a block scalar.
func yamlBlockLen(rest, line string) int {
	eol := strings.IndexByte(rest, '\n')
	if eol < 0 || (rest[0] != '|' && rest[0] != '>') {
		return 0
	}
	header := strings.TrimRight(rest[1:eol], " \t\r")
	if i := strings.Index(header, " #"); i >= 0 {
		header = strings.TrimRight(header[:i], " \t")
	}
	if strings.Trim(header, "+-0123456789") != "" {
		return 0
	}
	indent := len(line) - len(strings.TrimLeft(line, " "))
	end, pos := 0, eol+1
	for pos <= len(rest) {
		next := strings.IndexByte(rest[pos:], '\n')
		lineEnd := len(rest)
		if next >= 0 {
			lineEnd = pos + next
		}
		body := rest[pos:lineEnd]
		if strings.TrimSpace(body) != "" {
			if len(body)-len(strings.TrimLeft(body, " ")) <= indent {
				break
			}
			end = lineEnd
		}
		if next < 0 {
			break
		}
		pos = lineEnd + 1
	}
	if end == 0 {
		return 0
	}
	return len(strings.TrimRight(rest[:end], "\r"))
}

// yamlDecode returns the value a YAML or JSON reader takes from a quoted scalar, and false when raw is
// not one.
func yamlDecode(raw string) (string, bool) {
	if len(raw) < 2 || (raw[0] != '"' && raw[0] != '\'') || raw[len(raw)-1] != raw[0] {
		return "", false
	}
	if raw[0] == '\'' {
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), true
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		return s, true
	}
	return doubleQuoted(raw[1 : len(raw)-1]), true
}

// yamlValueDecode returns what a program receives from raw, the value of a name: value assignment as
// written, read inside closer.
func yamlValueDecode(raw string, closer byte) string {
	if (raw[0] == '|' || raw[0] == '>') && strings.IndexByte(raw, '\n') >= 0 {
		return blockDecode(raw)
	}
	if v, ok := yamlDecode(raw); ok {
		return v
	}
	if raw[0] == '"' || raw[0] == '\'' {
		// Text joined onto a quoted scalar is not YAML, and the shell's reading of the word is the
		// one a program running it receives.
		return shellValue(raw, closer)
	}
	// A plain scalar ends where a comment begins. The comment is masked with the value all the
	// same, because text that did not parse may hold a password with a space and a hash in it, and
	// the value as written stays among the readings the masker holds.
	for i := 1; i < len(raw); i++ {
		if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = strings.TrimRight(raw[:i], " \t")
			break
		}
	}
	if closer == '"' {
		return doubleQuoted(raw)
	}
	return raw
}

// blockDecode returns the lines of a block scalar without the indicator and with the indentation of
// its first line removed, which is the text a YAML reader hands the program.
func blockDecode(raw string) string {
	lines := strings.Split(raw, "\n")[1:]
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			indent = len(l) - len(strings.TrimLeft(l, " "))
			break
		}
	}
	for i, l := range lines {
		if len(l) >= indent && indent > 0 {
			lines[i] = l[indent:]
		}
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return strings.Join(lines, "\n")
}
