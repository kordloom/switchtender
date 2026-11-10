package wallcheck

import (
	"go/scanner"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// sep joins tokens and lines in hashed bodies. It cannot appear in Go source outside a literal.
const sep = "\x1f"

// lexeme is one Go token and its text.
type lexeme struct {
	// tok is the token class.
	tok token.Token
	// text is the literal text, or the operator or keyword spelling.
	text string
}

// lex returns the Go tokens of src with comments and semicolons dropped, so layout, comments, and
// automatic semicolon insertion do not change the result.
func lex(src []byte) []lexeme {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var out []lexeme
	for {
		_, tok, lit := s.Scan()
		switch {
		case tok == token.EOF:
			return out
		case tok == token.SEMICOLON:
			continue
		case lit != "":
			out = append(out, lexeme{tok: tok, text: lit})
		default:
			out = append(out, lexeme{tok: tok, text: tok.String()})
		}
	}
}

// exactBody joins the token texts.
func exactBody(toks []lexeme) []byte {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	return []byte(strings.Join(parts, sep))
}

// alphaBody joins the token texts with every identifier that is not predeclared renamed to its
// order of first appearance, so a copy with renamed variables, fields, or packages hashes the same.
func alphaBody(toks []lexeme) []byte {
	names := make(map[string]string)
	parts := make([]string, len(toks))
	for i, t := range toks {
		text := t.text
		if t.tok == token.IDENT && types.Universe.Lookup(text) == nil {
			n, ok := names[text]
			if !ok {
				n = "v" + strconv.Itoa(len(names))
				names[text] = n
			}
			text = n
		}
		parts[i] = text
	}
	return []byte(strings.Join(parts, sep))
}

// line is one significant line of a text file.
type line struct {
	// text is the line with surrounding whitespace trimmed and inner runs collapsed to one space.
	text string
	// num is the line's one-based number in the file.
	num int
}

// commentPrefixes open a comment line in Go, JavaScript, shell, YAML, or HTML. A line starting with
// one is not significant, so a dropped build tag or rewritten header does not change a hash.
var commentPrefixes = []string{"//", "/*", "*", "#", "<!--"}

// significant returns the lines of src that are neither blank nor comments, normalized for
// whitespace.
func significant(src []byte) []line {
	var out []line
	for i, raw := range strings.Split(string(src), "\n") {
		text := strings.Join(strings.Fields(raw), " ")
		if text == "" || hasCommentPrefix(text) {
			continue
		}
		out = append(out, line{text: text, num: i + 1})
	}
	return out
}

// hasCommentPrefix reports whether text opens with a comment marker.
func hasCommentPrefix(text string) bool {
	for _, p := range commentPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// joinLines joins the texts of lines for hashing.
func joinLines(lines []line) []byte {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.text
	}
	return []byte(strings.Join(parts, sep))
}
