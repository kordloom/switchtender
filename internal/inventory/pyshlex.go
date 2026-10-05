package inventory

import (
	"errors"
	"strings"
)

// errShlexQuote and errShlexEscape are the two ways Python's shlex.split fails on an INI host line,
// with the messages Python raises, lowercased.
var (
	// errShlexQuote is an opening quote with no closing one.
	errShlexQuote = errors.New("no closing quotation")
	// errShlexEscape is a backslash at the very end of the line.
	errShlexEscape = errors.New("no escaped character")
)

// shlexState is where Python's shlex lexer is within the line.
type shlexState byte

// The lexer states, named as Python's shlex names them.
const (
	// shlexSpace is between words.
	shlexSpace shlexState = ' '
	// shlexWord is inside a word.
	shlexWord shlexState = 'a'
	// shlexSingle is inside single quotes.
	shlexSingle shlexState = '\''
	// shlexDouble is inside double quotes.
	shlexDouble shlexState = '"'
	// shlexEscape is just after a backslash.
	shlexEscape shlexState = '\\'
	// shlexDone is past the end of the line.
	shlexDone shlexState = 0
)

// pyShlexSplit splits an INI host line into words exactly as Ansible does, with Python's
// shlex.split(line, comments=True) in POSIX mode. Spaces, tabs, carriage returns, and line feeds
// separate words; quotes group them and are removed; a backslash escapes the next character outside
// single quotes, and only a quote or a backslash inside double quotes; and a # outside quotes ends
// the line, even in the middle of a word, which keeps the word read so far.
func pyShlexSplit(line string) ([]string, error) {
	rs := []rune(line)
	var (
		out   []string
		pos   int
		state = shlexSpace
	)
	for state != shlexDone {
		word, quoted, next, err := shlexToken(rs, pos, &state)
		if err != nil {
			return nil, err
		}
		pos = next
		if word == "" && !quoted {
			break
		}
		out = append(out, word)
	}
	return out, nil
}

// shlexWhitespace reports whether r separates shlex words.
func shlexWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n'
}

// shlexToken reads one token from rs starting at pos, as Python's shlex.read_token does with
// whitespace_split set, and returns it, whether any part of it was quoted, and where reading
// stopped. state carries across calls the way the lexer's own state does.
func shlexToken(rs []rune, pos int, state *shlexState) (string, bool, int, error) {
	var (
		token        strings.Builder
		quoted       bool
		escapedState = shlexSpace
	)
	for {
		var next rune
		eof := pos >= len(rs)
		if !eof {
			next = rs[pos]
			pos++
		}
		switch *state {
		case shlexSpace:
			switch {
			case eof:
				*state = shlexDone
				return token.String(), quoted, pos, nil
			case shlexWhitespace(next):
				if token.Len() > 0 || quoted {
					return token.String(), quoted, pos, nil
				}
			case next == '#':
				// The comment runs to the end of the line, which is the end of the input here.
				pos = len(rs)
			case next == '\\':
				escapedState = shlexWord
				*state = shlexEscape
			case next == '\'':
				*state = shlexSingle
			case next == '"':
				*state = shlexDouble
			default:
				token.WriteRune(next)
				*state = shlexWord
			}
		case shlexSingle, shlexDouble:
			quoted = true
			switch {
			case eof:
				return "", quoted, pos, errShlexQuote
			case next == rune(*state):
				*state = shlexWord
			case next == '\\' && *state == shlexDouble:
				escapedState = *state
				*state = shlexEscape
			default:
				token.WriteRune(next)
			}
		case shlexEscape:
			if eof {
				return "", quoted, pos, errShlexEscape
			}
			// In POSIX mode a backslash inside double quotes escapes only the quote and itself; before
			// any other character it is kept.
			if escapedState == shlexDouble && next != '\\' && next != '"' {
				token.WriteRune('\\')
			}
			token.WriteRune(next)
			*state = escapedState
		case shlexWord:
			switch {
			case eof:
				*state = shlexDone
				return token.String(), quoted, pos, nil
			case shlexWhitespace(next):
				*state = shlexSpace
				if token.Len() > 0 || quoted {
					return token.String(), quoted, pos, nil
				}
			case next == '#':
				pos = len(rs)
				*state = shlexSpace
				if token.Len() > 0 || quoted {
					return token.String(), quoted, pos, nil
				}
			case next == '\'' || next == '"':
				*state = shlexState(next)
			case next == '\\':
				escapedState = shlexWord
				*state = shlexEscape
			default:
				token.WriteRune(next)
			}
		}
	}
}
