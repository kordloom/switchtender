package inventory

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ansible's INI plugin types every variable value with Python's ast.literal_eval and keeps the text
// as a string when literal_eval raises ValueError or SyntaxError. This file reproduces that: the
// value is read as a Python expression, and when the whole of it is made of literals (strings,
// numbers, True, False, None, tuples, lists, and dicts) the literal is the value, and when it is
// anything else the text is.
//
// A few literals Python accepts are refused rather than imitated, because their result is not
// stable across the Python and Ansible releases the engine is checked against or cannot be carried
// in the JSON a composed inventory is rendered as: complex numbers, sets, bytes, the ellipsis,
// named Unicode escapes, lone surrogates, infinite floats, integers past Python's conversion limit,
// and mapping keys that are not strings or integers. Each one makes the inventory need Ansible,
// which is the fail-closed answer: the engine never guesses at a value Ansible could read
// differently.

// errNotLiteral reports that literal_eval would raise, so the value is the text as written.
var errNotLiteral = errors.New("not a Python literal")

// maxLiteralDepth bounds how deeply a literal may nest. Python's own parser stops at two hundred
// levels; a value nested a hundred deep is no inventory variable.
const maxLiteralDepth = 100

// pyLiteralKind classifies a literal token.
type pyLiteralKind int

// The token kinds the literal lexer produces.
const (
	// litEOF is the end of the input.
	litEOF pyLiteralKind = iota
	// litNumber is an int or float literal.
	litNumber
	// litString is one string literal, before adjacent literals are joined.
	litString
	// litName is an identifier or keyword.
	litName
	// litOp is punctuation: ( ) [ ] { } , : + - and the ellipsis.
	litOp
)

// litToken is one token of a literal expression.
type litToken struct {
	// kind is what the token is.
	kind pyLiteralKind
	// text is the token's source text, or for a string its decoded value.
	text string
	// float marks a number token as a float rather than an int.
	float bool
	// base is an int token's radix.
	base int
	// fstring marks a string token written as an f-string or a template string, which literal_eval
	// does not accept.
	fstring bool
}

// pyLiteralEval returns the value Ansible's INI plugin gives the text src. It returns errNotLiteral
// when the text stays a string and a NeedsAnsibleError when the value is a literal the native
// engine refuses to imitate.
func pyLiteralEval(src string) (any, error) {
	src = strings.TrimLeft(src, " \t")
	toks, err := lexLiteral(src)
	if err != nil {
		return nil, err
	}
	p := &litParser{toks: toks}
	v, err := p.top()
	if err != nil {
		return nil, err
	}
	return v, nil
}

// lexLiteral splits src into literal tokens. Anything outside the literal grammar is reported as
// errNotLiteral, since Python would either fail to parse it or parse it into something literal_eval
// rejects. A token that only a refused literal could contain is reported as needing Ansible.
func lexLiteral(src string) ([]litToken, error) {
	if strings.ContainsRune(src, 0) {
		return nil, errNotLiteral
	}
	var toks []litToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '#':
			i = len(src)
		case c == '.' && strings.HasPrefix(src[i:], "..."):
			toks = append(toks, litToken{kind: litOp, text: "..."})
			i += 3
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			tok, n, err := lexNumber(src[i:])
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i += n
		case c == '\'' || c == '"':
			tok, n, err := lexString(src[i:], "")
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i += n
		case strings.IndexByte("()[]{},:+-", c) >= 0:
			toks = append(toks, litToken{kind: litOp, text: string(c)})
			i++
		default:
			r, size := utf8.DecodeRuneInString(src[i:])
			if !identStart(r) {
				return nil, errNotLiteral
			}
			j := i + size
			for j < len(src) {
				r, size := utf8.DecodeRuneInString(src[j:])
				if !identContinue(r) {
					break
				}
				j += size
			}
			name := src[i:j]
			if j < len(src) && (src[j] == '\'' || src[j] == '"') {
				prefix, ok := stringPrefix(name)
				if !ok {
					return nil, errNotLiteral
				}
				tok, n, err := lexString(src[j:], prefix)
				if err != nil {
					return nil, err
				}
				toks = append(toks, tok)
				i = j + n
				continue
			}
			toks = append(toks, litToken{kind: litName, text: name})
			i = j
		}
	}
	return append(toks, litToken{kind: litEOF}), nil
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isHexDigit reports whether c is an ASCII hexadecimal digit.
func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// identStart reports whether r may begin a Python identifier.
func identStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r)
}

// identContinue reports whether r may continue a Python identifier.
func identContinue(r rune) bool {
	return identStart(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) ||
		unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Pc, r)
}

// stringPrefix returns the normalized prefix of a string literal, or false when name is not one
// Python accepts. The result holds b for bytes, r for raw, and f for an f-string or template
// string.
func stringPrefix(name string) (string, bool) {
	lower := strings.ToLower(name)
	switch lower {
	case "r", "u", "b", "br", "rb", "f", "fr", "rf", "t", "tr", "rt":
	default:
		return "", false
	}
	out := ""
	if strings.Contains(lower, "b") {
		out += "b"
	}
	if strings.Contains(lower, "r") {
		out += "r"
	}
	if strings.ContainsAny(lower, "ft") {
		out += "f"
	}
	return out, true
}

// lexNumber reads one number literal at the start of s and returns its token and length, following
// Python's integer and float grammar: underscores only between digits, no leading zeros on a
// nonzero decimal integer, and an imaginary suffix refused. A number run straight into a name is
// not a literal.
func lexNumber(s string) (litToken, int, error) {
	i := 0
	digitsUnder := func(ok func(byte) bool) bool {
		start := i
		for i < len(s) {
			switch {
			case ok(s[i]):
				i++
			case s[i] == '_' && i+1 < len(s) && ok(s[i+1]) && i > start:
				i++
			default:
				return i > start
			}
		}
		return i > start
	}
	tok := litToken{kind: litNumber, base: 10}
	if len(s) > 1 && s[0] == '0' && strings.IndexByte("xXoObB", s[1]) >= 0 {
		var ok func(byte) bool
		switch s[1] {
		case 'x', 'X':
			ok, tok.base = isHexDigit, 16
		case 'o', 'O':
			ok, tok.base = func(c byte) bool { return c >= '0' && c <= '7' }, 8
		default:
			ok, tok.base = func(c byte) bool { return c == '0' || c == '1' }, 2
		}
		i = 2
		if i < len(s) && s[i] == '_' {
			i++
		}
		if !digitsUnder(ok) {
			return litToken{}, 0, errNotLiteral
		}
		tok.text = strings.ReplaceAll(s[2:i], "_", "")
		return finishNumber(s, i, tok)
	}
	intStart := i
	hasInt := digitsUnder(isDigit)
	intPart := s[intStart:i]
	if i < len(s) && s[i] == '.' {
		tok.float = true
		i++
		if i < len(s) && isDigit(s[i]) {
			digitsUnder(isDigit)
		} else if !hasInt {
			return litToken{}, 0, errNotLiteral
		}
	} else if !hasInt {
		return litToken{}, 0, errNotLiteral
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			i = j
			digitsUnder(isDigit)
			tok.float = true
		} else {
			return litToken{}, 0, errNotLiteral
		}
	}
	if !tok.float {
		trimmed := strings.ReplaceAll(intPart, "_", "")
		if len(trimmed) > 1 && trimmed[0] == '0' && strings.Trim(trimmed, "0") != "" {
			return litToken{}, 0, errNotLiteral
		}
	}
	tok.text = strings.ReplaceAll(s[:i], "_", "")
	return finishNumber(s, i, tok)
}

// finishNumber checks what follows a number literal ending at i: an imaginary suffix makes the
// value complex, which is refused, and a letter, digit, or underscore makes it no literal at all.
func finishNumber(s string, i int, tok litToken) (litToken, int, error) {
	if i < len(s) {
		if s[i] == 'j' || s[i] == 'J' {
			return litToken{}, 0, needsAnsible("a variable holds a Python complex number")
		}
		r, _ := utf8.DecodeRuneInString(s[i:])
		if identContinue(r) || s[i] == '.' {
			return litToken{}, 0, errNotLiteral
		}
	}
	return tok, i, nil
}

// lexString reads one string literal at the start of s, whose prefix has already been read, and
// returns its token, holding the decoded value, and its length.
func lexString(s, prefix string) (litToken, int, error) {
	raw := strings.Contains(prefix, "r")
	quote := s[:1]
	if strings.HasPrefix(s, quote+quote+quote) {
		quote = s[:3]
	}
	i := len(quote)
	var body strings.Builder
	for {
		if i >= len(s) {
			return litToken{}, 0, errNotLiteral
		}
		if strings.HasPrefix(s[i:], quote) {
			i += len(quote)
			break
		}
		c := s[i]
		if c != '\\' {
			body.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return litToken{}, 0, errNotLiteral
		}
		if raw {
			// A raw string keeps the backslash and the character after it, quote included, so an
			// escaped quote does not end it.
			body.WriteString(s[i : i+2])
			i += 2
			continue
		}
		n, err := writeEscape(&body, s[i:], strings.Contains(prefix, "b"))
		if err != nil {
			return litToken{}, 0, err
		}
		i += n
	}
	tok := litToken{kind: litString, text: body.String(), fstring: strings.Contains(prefix, "f")}
	if strings.Contains(prefix, "b") && !tok.fstring {
		return litToken{}, 0, needsAnsible("a variable holds a Python bytes literal")
	}
	return tok, i, nil
}

// writeEscape decodes the backslash escape at the start of s into body and returns its length.
func writeEscape(body *strings.Builder, s string, bytesLit bool) (int, error) {
	c := s[1]
	switch c {
	case '\\', '\'', '"':
		body.WriteByte(c)
		return 2, nil
	case 'a':
		body.WriteByte('\a')
		return 2, nil
	case 'b':
		body.WriteByte('\b')
		return 2, nil
	case 'f':
		body.WriteByte('\f')
		return 2, nil
	case 'n':
		body.WriteByte('\n')
		return 2, nil
	case 'r':
		body.WriteByte('\r')
		return 2, nil
	case 't':
		body.WriteByte('\t')
		return 2, nil
	case 'v':
		body.WriteByte('\v')
		return 2, nil
	case 'x':
		if len(s) < 4 || !isHexDigit(s[2]) || !isHexDigit(s[3]) {
			return 0, errNotLiteral
		}
		v, _ := strconv.ParseUint(s[2:4], 16, 8)
		body.WriteRune(rune(v))
		return 4, nil
	case 'N':
		if bytesLit {
			body.WriteString(s[:2])
			return 2, nil
		}
		return 0, needsAnsible("a variable uses a named Unicode escape")
	case 'u', 'U':
		if bytesLit {
			body.WriteString(s[:2])
			return 2, nil
		}
		width := 4
		if c == 'U' {
			width = 8
		}
		if len(s) < 2+width {
			return 0, errNotLiteral
		}
		for k := 2; k < 2+width; k++ {
			if !isHexDigit(s[k]) {
				return 0, errNotLiteral
			}
		}
		v, _ := strconv.ParseUint(s[2:2+width], 16, 32)
		if v > unicode.MaxRune {
			return 0, errNotLiteral
		}
		if v >= 0xd800 && v <= 0xdfff {
			return 0, needsAnsible("a variable holds a lone surrogate character")
		}
		body.WriteRune(rune(v))
		return 2 + width, nil
	}
	if c >= '0' && c <= '7' {
		n := 2
		for n < 4 && n < len(s) && s[n] >= '0' && s[n] <= '7' {
			n++
		}
		v, _ := strconv.ParseUint(s[1:n], 8, 32)
		body.WriteRune(rune(v))
		return n, nil
	}
	// Python keeps an escape it does not know, backslash and all, and only warns.
	body.WriteByte('\\')
	return 1, nil
}

// litParser is a recursive descent parser over literal tokens.
type litParser struct {
	// toks are the tokens, ending with litEOF.
	toks []litToken
	// pos is the next token to read.
	pos int
	// depth is how deeply the current value nests.
	depth int
}

// peek returns the next token without consuming it.
func (p *litParser) peek() litToken { return p.toks[p.pos] }

// next consumes and returns the next token.
func (p *litParser) next() litToken {
	t := p.toks[p.pos]
	if t.kind != litEOF {
		p.pos++
	}
	return t
}

// isOp reports whether the next token is the punctuation op.
func (p *litParser) isOp(op string) bool {
	t := p.peek()
	return t.kind == litOp && t.text == op
}

// litValue is a parsed value and whether it came straight from a number literal, parentheses
// aside, which is the only operand literal_eval accepts under a sign.
type litValue struct {
	// v is the value.
	v any
	// constNumber reports that v is a number written as a literal, not one a sign produced.
	constNumber bool
}

// top reads the whole expression: one value, or several separated by commas, which Python reads as
// a tuple, and nothing after it.
func (p *litParser) top() (any, error) {
	if p.peek().kind == litEOF {
		return nil, errNotLiteral
	}
	first, err := p.elem()
	if err != nil {
		return nil, err
	}
	if !p.isOp(",") {
		if p.peek().kind != litEOF {
			return nil, errNotLiteral
		}
		return first.v, nil
	}
	items := []any{first.v}
	for p.isOp(",") {
		p.next()
		if p.peek().kind == litEOF {
			break
		}
		v, err := p.elem()
		if err != nil {
			return nil, err
		}
		items = append(items, v.v)
	}
	if p.peek().kind != litEOF {
		return nil, errNotLiteral
	}
	return items, nil
}

// elem reads one value. A binary operator after it is never a literal here, since the only one
// literal_eval accepts builds a complex number, and the lexer has already refused those.
func (p *litParser) elem() (litValue, error) {
	v, err := p.unary()
	if err != nil {
		return litValue{}, err
	}
	if p.isOp("+") || p.isOp("-") {
		return litValue{}, errNotLiteral
	}
	return v, nil
}

// unary reads a value with at most one sign, which literal_eval accepts only on a number literal.
func (p *litParser) unary() (litValue, error) {
	if !p.isOp("+") && !p.isOp("-") {
		return p.atom()
	}
	sign := p.next().text
	v, err := p.atom()
	if err != nil {
		return litValue{}, err
	}
	n, ok := v.v.(pyNumber)
	if !ok || !v.constNumber {
		return litValue{}, errNotLiteral
	}
	if sign == "-" {
		n = negate(n)
	}
	return litValue{v: n}, nil
}

// negate returns -n.
func negate(n pyNumber) pyNumber {
	if n.float {
		f, _ := strconv.ParseFloat(n.text, 64)
		out, _ := pyFloat(-f)
		return out
	}
	v, _ := new(big.Int).SetString(n.text, 10)
	return pyIntFromBig(v.Neg(v))
}

// atom reads one literal: a number, joined string literals, True, False, None, the ellipsis, or a
// parenthesized value, tuple, list, or dict.
func (p *litParser) atom() (litValue, error) {
	t := p.next()
	switch t.kind {
	case litNumber:
		n, err := numberValue(t)
		return litValue{v: n, constNumber: true}, err
	case litString:
		s, err := p.strings(t)
		return litValue{v: s}, err
	case litName:
		switch t.text {
		case "True":
			return litValue{v: true}, nil
		case "False":
			return litValue{v: false}, nil
		case "None":
			return litValue{v: nil}, nil
		}
		// set() is the empty set, and an identifier that is not plain ASCII may normalize to set.
		if p.isOp("(") && (t.text == "set" || !isASCII(t.text)) {
			return litValue{}, needsAnsible("a variable holds a Python set")
		}
		return litValue{}, errNotLiteral
	case litOp:
		switch t.text {
		case "...":
			// ansible-core 2.19 and later turn the ellipsis into the text "...", and earlier releases
			// keep the Python object, which their listing cannot print.
			return litValue{}, needsAnsible("a variable holds a Python ellipsis")
		case "(":
			return p.group(")", true)
		case "[":
			return p.group("]", false)
		case "{":
			m, err := p.braces()
			return litValue{v: m}, err
		}
	}
	return litValue{}, errNotLiteral
}

// isASCII reports whether s holds only ASCII characters.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// numberValue converts a number token to its Python value.
func numberValue(t litToken) (any, error) {
	if t.float {
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil && !(errors.Is(err, strconv.ErrRange) && f == 0) {
			return nil, needsAnsible("a variable holds a float too large to carry")
		}
		n, ok := pyFloat(f)
		if !ok {
			return nil, needsAnsible("a variable holds a float too large to carry")
		}
		return n, nil
	}
	n, ok := pyInt(1, t.text, t.base)
	if !ok || len(n.text) > maxIntDigits {
		return nil, needsAnsible("a variable holds an integer longer than Python converts")
	}
	return n, nil
}

// strings joins first with every string literal right after it, the way Python reads adjacent
// literals as one. An f-string among them makes the whole a formatted string, which is not a
// literal.
func (p *litParser) strings(first litToken) (any, error) {
	var b strings.Builder
	b.WriteString(first.text)
	fstring := first.fstring
	for p.peek().kind == litString {
		t := p.next()
		fstring = fstring || t.fstring
		b.WriteString(t.text)
	}
	if fstring {
		return nil, errNotLiteral
	}
	return b.String(), nil
}

// group reads a parenthesized value or tuple, or a list, up to the closing punctuation. A pair of
// parentheses around a single value without a comma is that value, unchanged.
func (p *litParser) group(closing string, paren bool) (litValue, error) {
	if p.depth++; p.depth > maxLiteralDepth {
		return litValue{}, needsAnsible("a variable nests too deeply")
	}
	defer func() { p.depth-- }()
	if p.isOp(closing) {
		p.next()
		return litValue{v: []any{}}, nil
	}
	first, err := p.elem()
	if err != nil {
		return litValue{}, err
	}
	if paren && p.isOp(closing) {
		p.next()
		return first, nil
	}
	items := []any{first.v}
	for p.isOp(",") {
		p.next()
		if p.isOp(closing) {
			break
		}
		v, err := p.elem()
		if err != nil {
			return litValue{}, err
		}
		items = append(items, v.v)
	}
	if !p.isOp(closing) {
		return litValue{}, errNotLiteral
	}
	p.next()
	return litValue{v: items}, nil
}

// braces reads a dict, or refuses a set.
func (p *litParser) braces() (any, error) {
	if p.depth++; p.depth > maxLiteralDepth {
		return nil, needsAnsible("a variable nests too deeply")
	}
	defer func() { p.depth-- }()
	m := newOrderedMap()
	if p.isOp("}") {
		p.next()
		return m, nil
	}
	for {
		key, err := p.elem()
		if err != nil {
			return nil, err
		}
		if !p.isOp(":") {
			if p.isOp(",") || p.isOp("}") {
				return nil, needsAnsible("a variable holds a Python set")
			}
			return nil, errNotLiteral
		}
		p.next()
		value, err := p.elem()
		if err != nil {
			return nil, err
		}
		if !m.Set(key.v, value.v) {
			return nil, needsAnsible("a variable holds a mapping whose keys are not strings " +
				"or integers")
		}
		if !p.isOp(",") {
			break
		}
		p.next()
		if p.isOp("}") {
			break
		}
	}
	if !p.isOp("}") {
		return nil, errNotLiteral
	}
	p.next()
	return m, nil
}

// pyParseValue is Ansible's INI _parse_value: the literal when the text is one, and otherwise the
// text itself.
func pyParseValue(text string) (any, error) {
	v, err := pyLiteralEval(text)
	if errors.Is(err, errNotLiteral) {
		return text, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// pyFloatValue parses a decimal float the way Python's float() does, or reports false.
func pyFloatValue(text string) (float64, bool) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !(errors.Is(err, strconv.ErrRange) && f == 0) {
		return 0, false
	}
	return f, !math.IsInf(f, 0) && !math.IsNaN(f)
}
