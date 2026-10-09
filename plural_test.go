package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// lazyPlural matches a noun that hedges its own number, such as "job(s)" or "match(es)". A count
// already in the sentence decides the number, so the real plural is always knowable.
var lazyPlural = regexp.MustCompile(`[A-Za-z]+\((?:e?s)\)`)

// pluralSkipDirs are directories whose contents this project does not ship as text a person reads.
var pluralSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "testdata": true, "e2e": true, "jstest": true,
}

// sourceLiteral is one string literal found in a source file.
type sourceLiteral struct {
	// Line is the line the literal starts on.
	Line int
	// Text is the literal as written, quotes included.
	Text string
}

// TestNoTextHedgesItsPlural holds every sentence the product writes to one rule: a count is
// followed by the noun in the number it needs, never by "(s)".
//
// Go and JavaScript are read for their string literals alone, since a call such as f(s) is code
// and not a hedge. Documentation and the pages built from it are read whole. The site's other pages
// are held to the same rule by their own copy review and are not walked here.
func TestNoTextHedgesItsPlural(t *testing.T) {
	t.Parallel()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	var files []string
	for _, dir := range []string{"cmd", "internal", "witness", "site"} {
		walkErr := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry,
			err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if pluralSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			files = append(files, path)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
	docs, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	files = append(files, docs...)

	checked := 0
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		var literals []sourceLiteral
		switch {
		case strings.HasSuffix(rel, "_test.go"):
			continue
		case strings.HasSuffix(rel, ".go") && !strings.HasPrefix(rel, "site/"):
			literals = goStringLiterals(t, path, readSource(t, path))
		case strings.HasSuffix(rel, ".js") && !strings.HasSuffix(rel, ".test.js"):
			literals = jsStringLiterals(readSource(t, path))
		case strings.HasPrefix(rel, "docs/") && strings.HasSuffix(rel, ".md"),
			strings.HasPrefix(rel, "site/docs/") && strings.HasSuffix(rel, ".html"),
			strings.HasPrefix(rel, "internal/ui/templates/") && strings.HasSuffix(rel, ".html"):
			literals = wholeLines(readSource(t, path))
		default:
			continue
		}
		checked++
		for _, lit := range literals {
			for _, hedge := range lazyPlural.FindAllString(lit.Text, -1) {
				t.Errorf("%s:%d writes %q; spell the plural the count needs, with "+
					"util.Plural in Go or a plural helper in JavaScript:\n  %s", rel, lit.Line,
					hedge, strings.TrimSpace(lit.Text))
			}
		}
	}
	// A walk rooted in the wrong place reads nothing and passes, so it must have read something.
	if checked < 100 {
		t.Fatalf("checked %d files, want the repository's sources and docs", checked)
	}
}

// readSource reads a file the guard checks.
func readSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// wholeLines returns each line of a prose file as a literal, so every word in it is checked.
func wholeLines(src string) []sourceLiteral {
	var out []sourceLiteral
	for i, line := range strings.Split(src, "\n") {
		out = append(out, sourceLiteral{Line: i + 1, Text: line})
	}
	return out
}

// goStringLiterals returns every string literal in a Go source file, leaving out comments and
// code.
func goStringLiterals(t *testing.T, path, src string) []sourceLiteral {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), func(pos token.Position, msg string) {
		t.Errorf("scan %s: %s", pos, msg)
	}, 0)
	var out []sourceLiteral
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING {
			out = append(out, sourceLiteral{Line: fset.Position(pos).Line, Text: lit})
		}
	}
}

// jsRegexAfter are the keywords after which a slash opens a regular expression rather than
// dividing.
var jsRegexAfter = map[string]bool{
	"return": true, "typeof": true, "case": true, "do": true, "else": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "instanceof": true, "yield": true,
	"await": true,
}

// jsLexer walks JavaScript source far enough to find its string and template literals: it skips
// comments and regular expression literals, which may hold quote characters, and follows template
// substitutions, which may hold literals of their own.
type jsLexer struct {
	// src is the source being read.
	src string
	// i is the offset of the next byte to read.
	i int
	// line is the line the next byte sits on.
	line int
	// regexOK reports whether a slash at this point opens a regular expression.
	regexOK bool
	// braces holds, for each template substitution being read, how many braces it has open.
	braces []int
	// out collects the literals found.
	out []sourceLiteral
}

// jsStringLiterals returns every string and template literal in a JavaScript source. A template's
// text is returned without its substitutions, and each literal inside a substitution is returned
// on its own.
func jsStringLiterals(src string) []sourceLiteral {
	l := &jsLexer{src: src, line: 1, regexOK: true}
	l.run()
	return l.out
}

// run reads the source to its end.
func (l *jsLexer) run() {
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch {
		case c == '\n':
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case strings.HasPrefix(l.src[l.i:], "//"):
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
		case strings.HasPrefix(l.src[l.i:], "/*"):
			end := strings.Index(l.src[l.i+2:], "*/")
			if end < 0 {
				end = len(l.src) - l.i - 2
			}
			l.skip(l.i + 2 + end + 2)
		case c == '\'' || c == '"':
			l.quoted(c)
			l.regexOK = false
		case c == '`':
			l.i++
			l.template()
		case c == '/' && l.regexOK:
			l.regex()
			l.regexOK = false
		case c == '{' && len(l.braces) > 0:
			l.braces[len(l.braces)-1]++
			l.i++
			l.regexOK = true
		case c == '}' && len(l.braces) > 0 && l.braces[len(l.braces)-1] == 0:
			l.braces = l.braces[:len(l.braces)-1]
			l.i++
			l.template()
		case c == '}' && len(l.braces) > 0:
			l.braces[len(l.braces)-1]--
			l.i++
			l.regexOK = true
		case isJSWordByte(c):
			start := l.i
			for l.i < len(l.src) && isJSWordByte(l.src[l.i]) {
				l.i++
			}
			l.regexOK = jsRegexAfter[l.src[start:l.i]]
		default:
			l.regexOK = c != ')' && c != ']'
			l.i++
		}
	}
}

// skip moves to offset end, counting the lines passed.
func (l *jsLexer) skip(end int) {
	end = min(end, len(l.src))
	l.line += strings.Count(l.src[l.i:end], "\n")
	l.i = end
}

// quoted reads a single or double quoted string starting at the current offset.
func (l *jsLexer) quoted(quote byte) {
	start, line := l.i, l.line
	l.i++
	for l.i < len(l.src) && l.src[l.i] != quote && l.src[l.i] != '\n' {
		if l.src[l.i] == '\\' {
			l.i++
		}
		l.i++
	}
	l.i = min(l.i+1, len(l.src))
	l.out = append(l.out, sourceLiteral{Line: line, Text: l.src[start:l.i]})
}

// template reads template text from the current offset up to its closing backtick, or up to a
// substitution, which is then read as code until its closing brace returns here.
func (l *jsLexer) template() {
	start, line := l.i, l.line
	for l.i < len(l.src) {
		switch {
		case l.src[l.i] == '\\':
			l.skip(l.i + 2)
		case l.src[l.i] == '`':
			l.out = append(l.out, sourceLiteral{Line: line, Text: l.src[start:l.i]})
			l.i++
			l.regexOK = false
			return
		case strings.HasPrefix(l.src[l.i:], "${"):
			l.out = append(l.out, sourceLiteral{Line: line, Text: l.src[start:l.i]})
			l.i += 2
			l.braces = append(l.braces, 0)
			l.regexOK = true
			return
		default:
			l.skip(l.i + 1)
		}
	}
}

// regex reads a regular expression literal and its flags, so the quotes it may hold are not read
// as the start of a string.
func (l *jsLexer) regex() {
	l.i++
	class := false
	for l.i < len(l.src) && l.src[l.i] != '\n' {
		c := l.src[l.i]
		switch {
		case c == '\\':
			l.i++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			l.i++
			for l.i < len(l.src) && isJSWordByte(l.src[l.i]) {
				l.i++
			}
			return
		}
		l.i++
	}
}

// isJSWordByte reports whether a byte can be part of an identifier, keyword, or number.
func isJSWordByte(c byte) bool {
	return c == '_' || c == '$' || c == '.' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// TestJSStringLiteralsFindsTheHedgeAndNotTheCode pins that the JavaScript reader finds a hedge in
// any kind of literal and does not mistake code for one, so the guard above can neither pass by
// missing a string nor fail on a call written f(s).
func TestJSStringLiteralsFindsTheHedgeAndNotTheCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Src        string
		WantHedges []string
	}{{ // Test 0: A call taking s is code.
		Src: "function esc(s) { return pad(s, 2); }",
	}, { // Test 1: A hedge in a single and a double quoted string.
		Src:        `var a = n + ' signature(s).'; var b = n + " claim(s) after it";`,
		WantHedges: []string{"signature(s)", "claim(s)"},
	}, { // Test 2: A regular expression holding a quote does not open a string.
		Src: `return /[",\n]/.test(s) ? '"' + s + '"' : esc(s);`,
	}, { // Test 3: A hedge in template text, and in a string inside a substitution.
		Src:        "const t = `${n} job(s) and ${f(\"match(es)\")} left`; g(s);",
		WantHedges: []string{"job(s)", "match(es)"},
	}, { // Test 4: A comment is not text the product writes.
		Src: "// a function(s) comment\n/* block(s) */ x(s);",
	}, { // Test 5: Division is not a regular expression, so the string after it is read.
		Src:        `var r = total / count; var m = "proof(s)";`,
		WantHedges: []string{"proof(s)"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, lit := range jsStringLiterals(test.Src) {
				got = append(got, lazyPlural.FindAllString(lit.Text, -1)...)
			}
			if diff := cmp.Diff(test.WantHedges, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hedges mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
