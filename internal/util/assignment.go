package util

import (
	"regexp"
	"slices"
	"strings"
)

// iniAssignment finds one name=value assignment in free text: the name, a quote closing the name
// when it is written as a string, as in HCL's "password" = "x", and the first byte of the value. How
// far the value runs is the shell's question and not a pattern's, so the pattern stops there and
// shellWord answers it.
var iniAssignment = regexp.MustCompile(`(?i)([a-z0-9_][a-z0-9_.\-]*)(["']?)\s*=\s*(\S)`)

// yamlAssignment finds one name: value line in text that did not parse as a document, with the same
// three groups. A quote may close the name because a JSON body is written this way: curl -d
// '{"password":"x"}' carried a password no pattern saw, since the quote sat between the name and the
// colon, so the body went into every disclosure path in the clear and the masker never learned it.
var yamlAssignment = regexp.MustCompile(`(?i)([a-z0-9_][a-z0-9_.\-]*)(["']?)\s*:[ \t]*(\S)`)

// assignmentForm is one textual form an assignment takes.
type assignmentForm struct {
	// pattern finds a name, an optional quote closing it, and the first byte of the value.
	pattern *regexp.Regexp
	// read returns how many bytes of rest, the text from where the value starts, the value takes, and
	// whether its quotes stay around the mask. line is the text before the value on its own line, and
	// closer the quote that line leaves open, if any.
	read func(rest, line string, closer byte) (int, bool)
	// decode returns what a program receives from raw, the value as written.
	decode func(raw string, closer byte) string
	// inline holds the read and decode used instead for an assignment written inside a command line,
	// or is nil when read already reads the value as a command line does.
	inline *assignmentForm
}

// assignmentForms are the textual forms, read together in one scan of text no parser accepted and of
// the string leaves of text one did.
var assignmentForms = []assignmentForm{
	{pattern: iniAssignment, read: shellWordLen, decode: shellValue},
	{pattern: yamlAssignment, read: yamlValue, decode: yamlValueDecode,
		inline: &assignmentForm{read: yamlWordLen, decode: yamlWordDecode}},
}

// maxNestedAssignmentDepth bounds how deep a secret may be nested inside another assignment's value
// with no separating space, such as cmd=psql;password=x. Each level is a value that itself parses as
// an assignment; a human writes one or two, never many.
//
// The bound is what keeps the scan linear. A value that is really a long run of joined assignments,
// a=a=a=..., is an adversarial input, and without a limit rescanning each nested value turned
// redaction quadratic: a megabyte body pinned a core for minutes on the digest path every audited
// change runs through. Past the bound the remaining value is emitted unscanned.
//
// Eight rather than a rounder larger number because the depth is also a constant factor on the scan:
// each level re-reads the value below it, so a megabyte of joined assignments costs about 350ms here
// against 1.15s at thirty-two, measured, while still leaving several times the nesting any real
// command line has.
const maxNestedAssignmentDepth = 8

// Assignment is one secret-looking name=value pair found in free text, with its value unquoted so a
// caller holds the bare secret and can match it literally in output.
type Assignment struct {
	// Name is the variable the value was assigned to.
	Name string
	// Value is what a program receives from the assignment: the value with the quoting and escapes
	// of its form removed.
	Value string
	// Raw is the value as written, quotes and escapes included, for a reader that decodes it the way
	// the consuming program does.
	Raw string
}

// RedactAssignments replaces the value of every secret-looking assignment in text with mask, and
// reports the assignments it replaced.
//
// It lives here because two things need it and they must agree. An inventory carries these assignments
// as its content, and a run's variables carry them inside ordinary string values: a command line is a
// string, and a command line can hold a password. Redacting only the values under a secret-sounding key
// left the same secret in the clear whenever it sat inside a value under a name like deploy_cmd, so the
// string a reader was refused in an inventory shipped verbatim in a signed receipt.
//
// A non-secret name does not hide a secret joined onto its value: the value is itself scanned for a
// nested assignment, to a bounded depth, so a secret written after an ordinary one on the same line is
// still found. The scan never rewinds, so it stays linear in the length of the text.
func RedactAssignments(text, mask string) (string, []Assignment) {
	var found []Assignment
	text = redactDepth(text, mask, &found, maxNestedAssignmentDepth, false)
	// A credential can also ride inside a URL, as scheme://user:password@host, where no name=value
	// pattern sees it: pg_dump postgres://backup:pass@db and git clone https://x:token@host both
	// carry a secret no assignment names. Only the audit digest scrubbed this shape, so a receipt
	// showed the password redacted while the dossier mailed to an outside auditor, the evidence
	// page, and the text sent to an LLM all showed it plain, and the run-log masker never learned
	// the value so a set -x echoed it into the stored log. It is masked here so the one reading
	// every disclosure path shares closes all of them, and the stripped password is reported like
	// any other assignment so the masker matches it in output too.
	text = urlUserinfo.ReplaceAllStringFunc(text, func(m string) string {
		scheme := m[:strings.Index(m, "://")+3]
		userinfo := m[len(scheme) : len(m)-1]
		if i := strings.IndexByte(userinfo, ':'); i >= 0 && userinfo[i+1:] != "" {
			found = append(found, Assignment{Name: "url_userinfo", Value: userinfo[i+1:],
				Raw: userinfo[i+1:]})
		}
		return scheme + mask + "@"
	})
	return text, found
}

// urlUserinfo matches the user:password@ credential embedded in a URL-shaped string. The value the
// masker needs is the password after the colon, which the caller extracts from the match.
var urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/@\s]+@`)

// Readings returns every string the assigned value can reach a program as, Value first, leaving out
// empty ones. A run's output is masked by matching these literally, and a program prints a value the
// way it read it: a shell strips the quotes and escapes, Ansible reads an INI value as a Python
// literal, and a tool that echoes its own command line or configuration prints it as written.
// Ansible splits a host line as a shell would before it reads the literal, but reads a group's vars
// line as written, so 'multi\nline' under [db:vars] is a password holding a newline. Both literals
// are readings, since the text alone does not say which kind of line held the value.
func (a Assignment) Readings() []string {
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	add(a.Value)
	add(Unquote(a.Raw))
	if v, ok := PyUnquote(a.Value); ok {
		add(v)
	}
	if v, ok := PyUnquote(a.Raw); ok {
		add(v)
	}
	return out
}

// redactDepth applies every assignment form across text, replacing the values whose names the
// classifier calls secret and recording each one, with the remaining nesting budget carried
// explicitly. It makes a single forward pass that takes the leftmost assignment of any form, and for
// a non-secret assignment whose value may itself hold a joined assignment it scans that value once
// rather than rewinding the outer scan into it. The budget stops a value that is really a long chain
// of joined assignments from being descended into without end.
//
// The forms share the pass because each value's reader decides what its bytes mean, and a pass per
// form could not know what another form had read. The name=value pass counted the stray quote inside
// a name: value it never read, so pass:0"&pass="hunter2 left the password in the clear until a
// second redaction masked it.
//
// Redacting the result again changes nothing. A value ends where its syntax ends, at a space, a
// separator, the quote holding the text, or the end of a line, and a mask written in its place ends
// at that same boundary, so a second pass reads the mask as the whole value and writes it back. The
// redacted text is what gets stored, signed, and disclosed, and a record whose digest covers it has
// to come out the same on every path that redacts it.
//
// A name: value value is read one of two ways. One that opens its line is a YAML entry, whose plain
// scalar holds spaces and runs to the end of the line, so a passphrase in an inventory no parser
// accepted is masked whole. One written inside a command line is one shell word, as a name=value
// value is. Masking to the end of that line hid everything after the secret from every reader below
// admin and from the receipt digest, so echo password: x > file and echo password: x --limit all
// disclosed the same text under one digest. Inside a quote the line opened, or inside a value read
// the wide way, the text is a string handed whole to another program, such as a header to curl or a
// YAML scalar to ansible-playbook -e, which reads its value to the end of that string. wide carries
// that last case into a nested scan.
func redactDepth(text, mask string, found *[]Assignment, depth int, wide bool) string {
	var out strings.Builder
	// next holds each form's leftmost match at or after pos, with offsets into text, searched again
	// only once pos passes its start, and nil for a form with no match left. A match ahead of pos is
	// still the leftmost from pos, so no byte is searched twice by one form.
	next := make([][]int, len(assignmentForms))
	for i, form := range assignmentForms {
		next[i] = matchFrom(form.pattern, text, 0)
	}
	// quote is the quote open at pos, counted over the text between values since the last newline
	// outside one. A value already read is left out of the count, because its reader has decided what
	// its bytes mean, and after redaction a secret one is a mask with no quote or newline in it.
	// Counting a stray quote that a plain value took as an ordinary byte, as in pass:0"&pass:"hunter2,
	// read the next value as the end of a quoted string and left the password after it in the clear.
	var quote byte
	// lineStart is where the line holding pos begins. It moves forward with pos, so finding the line a
	// name sits on never scans back over the text, which on a long line of assignments read one word
	// at a time would make the scan quadratic.
	pos, lineStart := 0, 0
	for pos < len(text) {
		best := -1
		for i, form := range assignmentForms {
			if next[i] != nil && next[i][0] < pos {
				next[i] = matchFrom(form.pattern, text, pos)
			}
			if next[i] != nil && (best < 0 || next[i][0] < next[best][0]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		form, loc := assignmentForms[best], next[best]
		name := text[loc[2]:loc[3]]
		valueStart := loc[6]
		if i := strings.LastIndexByte(text[pos:loc[2]], '\n'); i >= 0 {
			lineStart = pos + i + 1
		}
		lead := text[lineStart:loc[2]]
		// valueLine is where the line holding the value begins, which is what says whether a quote is
		// open around it. An earlier line cannot leave one open for this one: a shell word and a YAML
		// scalar both end at the newline.
		valueLine := lineStart
		if i := strings.LastIndexByte(text[loc[2]:valueStart], '\n'); i >= 0 {
			valueLine = loc[2] + i + 1
		}
		line := text[valueLine:valueStart]
		between := text[pos:valueStart]
		if i := strings.LastIndexByte(between, '\n'); i >= 0 {
			quote, between = 0, between[i+1:]
		}
		quote = advanceQuote(quote, between)
		closer := quote
		// A value that opens with the quote the line left open is either the end of that quoted
		// string, so nothing was assigned, or a quote of its own the line's count got wrong, as an
		// apostrophe earlier in a sentence does. A quote that closes again is the second, and reading
		// it as the first left the password inside it in the clear.
		//
		// Text joined onto the end of the quoted string is the value after all: the shell hands
		// echo "a password="hunter2 the word a password=hunter2, and reading nothing assigned there
		// left the password in the clear. The quote is kept as written and the joined text is read
		// as a value outside any quote.
		if closer != 0 && text[valueStart] == closer {
			switch {
			case quotedEnd(text, valueStart, 0, closer == '"') > 0:
				closer = 0
			case valueStart+1 < len(text) && !wordEnds[text[valueStart+1]]:
				valueStart++
				line = text[valueLine:valueStart]
				closer, quote = 0, 0
			}
		}
		// A quote the line's count leaves open keeps the wide reading even when the value's own quote
		// showed the count wrong. Redacting the masked text again sees the same count with no quote on
		// the value to correct it, reads wide, and a narrower first reading came out different the
		// second time.
		wideHere := wide || quote != 0 || opensEntry(lead)
		read, decode := form.read, form.decode
		if form.inline != nil && !wideHere {
			read, decode = form.inline.read, form.inline.decode
		}
		n, keepQuotes := read(text[valueStart:], line, closer)
		valueEnd := valueStart + n
		value := text[valueStart:valueEnd]
		out.WriteString(text[pos:valueStart])
		switch {
		case value == "":
			// The value ends before it starts, so the name assigns nothing here and there is neither
			// anything to mask nor anything to report.
		case SecretKey(name):
			if keepQuotes {
				out.WriteString(value[:1] + mask + value[len(value)-1:])
			} else {
				out.WriteString(mask)
			}
			*found = append(*found, Assignment{Name: name, Value: decode(value, closer), Raw: value})
		case depth > 0 && (strings.IndexByte(value, '=') >= 0 || strings.IndexByte(value, ':') >= 0):
			// The value may be a secret joined onto this one, like a=psql;password=x or a yaml line
			// note: ... ansible_ssh_pass: x, so it can hold either separator. Scan it once, spending a
			// level of the budget, instead of rewinding the outer scan into it.
			//
			// The quote pair is peeled off before recursing and put back afterward. Left on, the
			// nested scan's unquoted alternative ran to the next whitespace and swallowed the
			// closing quote, which broke both outputs at once: the redacted text lost its quote
			// (CONN="a password=*** with nothing closing it), and the captured secret came back as
			// `hunter2"` rather than `hunter2`. That second one is the dangerous half. The captured
			// values are what scrubs a run's own output, so the scrubber searched the log for a
			// string with a trailing quote, never matched, and left the real secret in the log, the
			// events, and the live stream while the receipt showed it redacted.
			open, inner, close := splitQuoted(value)
			out.WriteString(open)
			out.WriteString(redactDepth(inner, mask, found, depth-1, wideHere || open != ""))
			out.WriteString(close)
		default:
			out.WriteString(value)
		}
		lineStart = valueLine
		if i := strings.LastIndexByte(value, '\n'); i >= 0 {
			lineStart = valueStart + i + 1
		}
		pos = valueEnd
	}
	out.WriteString(text[pos:])
	return out.String()
}

// opensEntry reports whether lead, the text before a name on its line, leaves the name opening a
// YAML mapping entry: nothing before it but indentation, the dash of each sequence item, and the
// quote of a quoted name. It reads lead from the right and stops at the first other byte, so the
// cost is the indentation and not the line.
func opensEntry(lead string) bool {
	i := len(lead)
	if i > 0 && (lead[i-1] == '"' || lead[i-1] == '\'') {
		i--
	}
	for ; i > 0; i-- {
		switch lead[i-1] {
		case ' ', '\t':
		case '-':
			// A dash marks a sequence item only with a space after it. Joined to the name, as in
			// --token: x, it is part of a command line's option.
			if i == len(lead) || (lead[i] != ' ' && lead[i] != '\t') {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// matchFrom returns the leftmost match of pattern in text at or after from, with its offsets into
// text, or nil when there is none.
func matchFrom(pattern *regexp.Regexp, text string, from int) []int {
	loc := pattern.FindStringSubmatchIndex(text[from:])
	for i := range loc {
		if loc[i] >= 0 {
			loc[i] += from
		}
	}
	return loc
}

// shellSeparators end an unquoted value on a command line. A value that is not held open by a quote
// cannot contain one of these, because the shell would have ended the word there too. Reported with
// the separator, a value is longer than the secret, and since callers match it literally against a
// run's output, a tool that echoed the secret put it in the stored log while the receipt showed it
// redacted.
const shellSeparators = ";&|"

// splitQuoted separates a value's surrounding quote pair from its contents, returning empty
// delimiters for a value that is not quoted. It is the inverse of leaving the quotes on, which is
// what let a nested scan run past the closing one.
func splitQuoted(value string) (open, inner, close string) {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[:1], value[1 : len(value)-1], value[len(value)-1:]
	}
	return "", value, ""
}

// Unquote strips one matching pair of surrounding quotes, so a caller holds the bare value and can
// match it literally in a run's output. Keeping the quotes would have it searching for a string the
// output never contains.
func Unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}
