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
}

// assignmentForms are the textual forms, applied in order to text no parser accepted and to the
// string leaves of text one did.
var assignmentForms = []assignmentForm{
	{pattern: iniAssignment, read: shellWordLen, decode: shellValue},
	{pattern: yamlAssignment, read: yamlValue, decode: yamlValueDecode},
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
	for _, form := range assignmentForms {
		text = redactPatternDepth(form, text, mask, &found, maxNestedAssignmentDepth)
	}
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

// redactPatternDepth applies one assignment form across text, replacing the values whose names the
// classifier calls secret and recording each one, with the remaining nesting budget carried
// explicitly. It makes a single forward pass, and for a non-secret assignment whose value may itself
// hold a joined assignment it scans that value once rather than rewinding the outer scan into it. The
// budget stops a value that is really a long chain of joined assignments from being descended into
// without end.
//
// Redacting the result again changes nothing. A value ends where its syntax ends, at a space, a
// separator, the quote holding the text, or the end of a line, and a mask written in its place ends
// at that same boundary, so a second pass reads the mask as the whole value and writes it back. The
// redacted text is what gets stored, signed, and disclosed, and a record whose digest covers it has
// to come out the same on every path that redacts it.
func redactPatternDepth(form assignmentForm, text, mask string, found *[]Assignment, depth int) string {
	var out strings.Builder
	pos := 0
	for pos < len(text) {
		loc := form.pattern.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		name := text[pos+loc[2] : pos+loc[3]]
		valueStart := pos + loc[6]
		line := lineBefore(text, valueStart)
		closer := openQuote(line)
		// A value that opens with the quote the line left open is either the end of that quoted
		// string, so nothing was assigned, or a quote of its own the line's count got wrong, as an
		// apostrophe earlier in a sentence does. A quote that closes again is the second, and reading
		// it as the first left the password inside it in the clear.
		if closer != 0 && text[valueStart] == closer && quotedEnd(text, valueStart, 0, closer == '"') > 0 {
			closer = 0
		}
		n, keepQuotes := form.read(text[valueStart:], line, closer)
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
			*found = append(*found, Assignment{Name: name, Value: form.decode(value, closer), Raw: value})
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
			out.WriteString(redactPatternDepth(form, inner, mask, found, depth-1))
			out.WriteString(close)
		default:
			out.WriteString(value)
		}
		pos = valueEnd
	}
	out.WriteString(text[pos:])
	return out.String()
}

// shellSeparators end an unquoted value on a command line. A value that is not held open by a quote
// cannot contain one of these, because the shell would have ended the word there too. Reported with
// the separator, a value is longer than the secret, and since callers match it literally against a
// run's output, a tool that echoed the secret put it in the stored log while the receipt showed it
// redacted.
const shellSeparators = ";&|"

// lineBefore returns what precedes valueStart on its own line, which is what says whether a quote is
// open around the value. An earlier line cannot leave one open for this one: a shell word and a YAML
// scalar both end at the newline.
func lineBefore(text string, valueStart int) string {
	if i := strings.LastIndexByte(text[:valueStart], '\n'); i >= 0 {
		return text[i+1 : valueStart]
	}
	return text[:valueStart]
}

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
