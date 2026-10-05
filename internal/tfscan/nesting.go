package tfscan

// maxNestingDepth bounds how deeply brackets, braces, and parentheses may nest in a configuration
// file the scan parses. The HCL parsers, native syntax and JSON alike, are recursive descent with no
// depth guard, so a file of a million open brackets, well under the per-file byte ceiling, drives one
// past Go's goroutine-stack limit and aborts the process with a stack overflow that no recover
// catches. A real configuration nests a handful of levels, so this sits far above anything legitimate
// and far below where the parser's recursion grows dangerous.
const maxNestingDepth = 1000

// withinNestingBound reports whether src nests brackets, braces, and parentheses no deeper than
// maxNestingDepth, counting only those that structure the document: a bracket inside a string, a
// comment, or a heredoc body is literal text and is skipped. It is a cheap single pass over the bytes
// run before the HCL parser, so a file crafted to overflow the parser is left unread and fails closed
// rather than crashing the process. It never reports a shallow file as too deep, so it refuses only a
// file whose real nesting no configuration has.
//
// It is deliberately approximate inside strings and heredocs: it skips their contents whole, so it
// neither counts a bracket written there nor follows HCL interpolation into them. Interpolation does
// not nest to anywhere near the bound in any real file, and the bound exists to stop pathological
// depth, not to lex HCL.
func withinNestingBound(src []byte) bool {
	depth := 0
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			i = skipQuoted(src, i)
		case c == '#':
			i = skipLine(src, i)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			i = skipLine(src, i)
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i = skipBlockComment(src, i+2)
		case c == '<' && i+1 < len(src) && src[i+1] == '<':
			i = skipHeredoc(src, i)
		case c == '(' || c == '[' || c == '{':
			depth++
			if depth > maxNestingDepth {
				return false
			}
			i++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return true
}

// skipQuoted returns the index just past the double-quoted string that starts at the quote at i,
// honoring backslash escapes. An unterminated string is skipped to the end of src.
func skipQuoted(src []byte, i int) int {
	for i++; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(src)
}

// skipLine returns the index of the newline that ends the line at i, or the end of src.
func skipLine(src []byte, i int) int {
	for ; i < len(src); i++ {
		if src[i] == '\n' {
			return i
		}
	}
	return len(src)
}

// skipBlockComment returns the index just past the block comment whose body starts at i, or the end
// of src for an unterminated one.
func skipBlockComment(src []byte, i int) int {
	for ; i < len(src); i++ {
		if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
			return i + 2
		}
	}
	return len(src)
}

// skipHeredoc returns the index just past a heredoc that starts with << at i, skipping its whole
// body. It recognizes <<TAG and <<-TAG, reads TAG to the end of the line, and skips until a line
// whose trimmed content is TAG. When what follows << is not a heredoc introducer, it returns i+1 so
// the scan goes on from the next byte.
func skipHeredoc(src []byte, i int) int {
	j := i + 2
	if j < len(src) && src[j] == '-' {
		j++
	}
	start := j
	for j < len(src) && isTagByte(src[j]) {
		j++
	}
	tag := src[start:j]
	if len(tag) == 0 {
		return i + 1
	}
	j = skipLine(src, j)
	for j < len(src) {
		j++ // past the newline
		lineStart := j
		j = skipLine(src, j)
		if string(trimSpace(src[lineStart:j])) == string(tag) {
			return j
		}
	}
	return len(src)
}

// isTagByte reports whether c may appear in a heredoc tag, which is an identifier.
func isTagByte(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// trimSpace returns b without leading or trailing spaces and tabs.
func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
