package util

import (
	"regexp"
	"strings"
)

// iniAssignment matches one name=value assignment in free text. The name is captured so a single
// classifier decides whether it is secret, and the value is either a quoted run (spaces allowed, up to
// the closing quote) or an unquoted run up to the next whitespace, which is exactly what the INI form
// permits: several host variables share one line, so an unquoted value cannot contain a space.
var iniAssignment = regexp.MustCompile(
	`(?i)([a-z0-9_][a-z0-9_.\-]*)\s*=\s*("[^"\n]*"|'[^'\n]*'|[^\s]+)`)

// yamlAssignment matches one name: value line in text that did not parse as a document. The value runs
// to the end of the line rather than to the next space, because a YAML scalar needs no quotes to
// contain spaces and stopping at the first space left the rest of a passphrase in the clear.
var yamlAssignment = regexp.MustCompile(
	`(?i)([a-z0-9_][a-z0-9_.\-]*)\s*:[ \t]*("[^"\n]*"|'[^'\n]*'|[^\r\n]+)`)

// assignmentPatterns are the textual forms, applied in order to text no parser accepted and to the
// string leaves of text one did.
var assignmentPatterns = []*regexp.Regexp{iniAssignment, yamlAssignment}

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
	// Value is what it was assigned, without surrounding quotes.
	Value string
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
	for _, pattern := range assignmentPatterns {
		text = redactPattern(pattern, text, mask, &found)
	}
	return text, found
}

// redactPattern applies one assignment pattern across text, replacing the values whose names the
// classifier calls secret and recording each one.
func redactPattern(pattern *regexp.Regexp, text, mask string, found *[]Assignment) string {
	return redactPatternDepth(pattern, text, mask, found, maxNestedAssignmentDepth)
}

// redactPatternDepth is redactPattern with the remaining nesting budget carried explicitly. It makes a
// single forward pass, and for a non-secret assignment whose value may itself hold a joined assignment
// it scans that value once rather than rewinding the outer scan into it. The budget stops a value that
// is really a long chain of joined assignments from being descended into without end.
func redactPatternDepth(pattern *regexp.Regexp, text, mask string, found *[]Assignment, depth int) string {
	var out strings.Builder
	pos := 0
	for pos < len(text) {
		loc := pattern.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		name := text[pos+loc[2] : pos+loc[3]]
		valueStart, valueEnd := pos+loc[4], pos+loc[5]
		value := text[valueStart:valueEnd]
		out.WriteString(text[pos:valueStart])
		switch {
		case SecretKey(name):
			out.WriteString(mask)
			*found = append(*found, Assignment{Name: name, Value: Unquote(value)})
		case depth > 0 && (strings.IndexByte(value, '=') >= 0 || strings.IndexByte(value, ':') >= 0):
			// The value may be a secret joined onto this one, like a=psql;password=x or a yaml line
			// note: ... ansible_ssh_pass: x, so it can hold either separator. Scan it once, spending a
			// level of the budget, instead of rewinding the outer scan into it.
			out.WriteString(redactPatternDepth(pattern, value, mask, found, depth-1))
		default:
			out.WriteString(value)
		}
		pos = valueEnd
	}
	out.WriteString(text[pos:])
	return out.String()
}

// Unquote strips one matching pair of surrounding quotes, so a caller holds the bare value and matches
// it literally in output. Keeping the quotes would mask a string the output never contains.
func Unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}
