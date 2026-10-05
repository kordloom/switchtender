package inventory

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reproduces how Ansible turns a host entry into host names and a port: its
// parse_address, which finds a port only when what is left is a valid host name or address, and its
// expand_hostname_range, which expands [begin:end] and [begin:end:step] ranges, numeric with the
// begin's zero padding or alphabetic over ASCII letters.

// asciiLetters is Python's string.ascii_letters, the alphabet an alphabetic range walks.
const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// expandHostPattern is Ansible's base _expand_hostpattern: it splits a port from the entry when
// parse_address accepts it, and expands any range in what remains. limit bounds how many names
// the expansion may produce.
func expandHostPattern(entry string, limit int) ([]string, string, error) {
	pattern, port, ok := parseAddress(entry)
	if !ok {
		pattern, port = entry, ""
	}
	if !strings.Contains(pattern, "[") {
		return []string{pattern}, port, nil
	}
	names, err := expandHostnameRange(pattern, limit)
	if err != nil {
		return nil, "", err
	}
	return names, port, nil
}

// parseAddress is Ansible's parse_address with ranges allowed. It returns the host part and the
// port, and false when the entry is not a host name, IPv4 address, or IPv6 address once a port is
// taken off, in which case Ansible keeps the entry whole and with no port.
func parseAddress(address string) (string, string, bool) {
	port := ""
	if host, p, ok := bracketedHostPort(address); ok {
		address, port = host, p
	}
	if host, p, ok := hostPort(address); ok {
		address, port = host, p
	}
	if isIPv4Pattern(address) || isIPv6Pattern(address) || isHostnamePattern(address) {
		return address, port, true
	}
	return "", "", false
}

// bracketedHostPort matches [host]:port, where the port is decimal digits.
func bracketedHostPort(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "[") {
		return "", "", false
	}
	end := strings.LastIndex(s, "]:")
	if end < 2 || !allDigits(s[end+2:]) {
		return "", "", false
	}
	return s[1:end], s[end+2:], true
}

// hostPort matches a host made of characters other than colons and brackets, or complete
// bracketed expressions, followed by :port.
func hostPort(s string) (string, string, bool) {
	i := 0
	for i < len(s) {
		switch s[i] {
		case ':':
			if i+1 < len(s) && allDigits(s[i+1:]) {
				return s[:i], s[i+1:], true
			}
			return "", "", false
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return "", "", false
			}
			i += end + 1
		case ']':
			return "", "", false
		default:
			i++
		}
	}
	return "", "", false
}

// allDigits reports whether s is one or more ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// rangeEnd returns the length of a range expression [begin:end] or [begin:end:step] at the start
// of s, where begin and end are each matched by part and step is decimal digits, or zero when there
// is none.
func rangeEnd(s string, part func(string) int) int {
	if !strings.HasPrefix(s, "[") {
		return 0
	}
	i := 1
	n := part(s[i:])
	if n == 0 || i+n >= len(s) || s[i+n] != ':' {
		return 0
	}
	i += n + 1
	n = part(s[i:])
	if n == 0 {
		return 0
	}
	i += n
	if i < len(s) && s[i] == ':' {
		j := i + 1
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if j == i+1 {
			return 0
		}
		i = j
	}
	if i >= len(s) || s[i] != ']' {
		return 0
	}
	return i + 1
}

// runOf returns the length of the run at the start of s whose bytes satisfy ok.
func runOf(s string, ok func(byte) bool) int {
	n := 0
	for n < len(s) && ok(s[n]) {
		n++
	}
	return n
}

// numericRange is Ansible's numeric_range: [digits:digits] with an optional :step.
func numericRange(s string) int {
	return rangeEnd(s, func(t string) int { return runOf(t, isDigit) })
}

// hexadecimalRange is Ansible's hexadecimal_range, matched without regard to case.
func hexadecimalRange(s string) int {
	return rangeEnd(s, func(t string) int { return runOf(t, isHexDigit) })
}

// alphanumericRange is Ansible's alphanumeric_range: one letter to one letter, or digits to
// digits, with an optional :step, matched without regard to case.
func alphanumericRange(s string) int {
	if n := rangeEnd(s, func(t string) int {
		if t != "" && isASCIILetter(t[0]) {
			return 1
		}
		return 0
	}); n > 0 {
		// Both ends must be letters, not one letter and one digit run.
		inner := s[1 : n-1]
		parts := strings.Split(inner, ":")
		if len(parts[0]) == 1 && len(parts[1]) == 1 && isASCIILetter(parts[0][0]) &&
			isASCIILetter(parts[1][0]) {
			return n
		}
	}
	return numericRange(s)
}

// isASCIILetter reports whether c is an ASCII letter.
func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// isIPv4Pattern is Ansible's ipv4 pattern: four parts of 0 to 255, each possibly a numeric range.
func isIPv4Pattern(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if n := numericRange(p); n > 0 && n == len(p) {
			continue
		}
		if !ipv4Octet(p) {
			return false
		}
	}
	return true
}

// ipv4Octet matches [01]?[0-9]{1,2}, 2[0-4][0-9], or 25[0-5].
func ipv4Octet(p string) bool {
	if !allDigits(p) || len(p) > 3 {
		return false
	}
	if len(p) < 3 {
		return true
	}
	switch p[0] {
	case '0', '1':
		return true
	case '2':
		return p[1] < '5' || (p[1] == '5' && p[2] <= '5')
	}
	return false
}

// ipv6Component matches one IPv6 group: one to four hex digits, or a hexadecimal range.
func ipv6Component(s string) bool {
	if n := hexadecimalRange(s); n > 0 && n == len(s) {
		return true
	}
	return len(s) >= 1 && len(s) <= 4 && runOf(s, isHexDigit) == len(s)
}

// isIPv6Pattern is Ansible's ipv6 pattern: the uncompressed and compressed forms of an IPv6
// address, including the IPv4-in-IPv6 forms, with ranges allowed in place of components.
func isIPv6Pattern(s string) bool {
	if strings.Contains(s, ".") {
		return ipv6WithIPv4(s)
	}
	head, tail, compressed := strings.Cut(s, "::")
	if !compressed {
		groups := strings.Split(s, ":")
		if len(groups) != 8 {
			return false
		}
		return allComponents(groups)
	}
	if strings.Contains(tail, "::") {
		return false
	}
	var left, right []string
	if head != "" {
		left = strings.Split(head, ":")
	}
	if tail != "" {
		right = strings.Split(tail, ":")
	}
	if !allComponents(left) || !allComponents(right) {
		return false
	}
	// The pattern allows up to seven groups around the gap, but never all seven on one side.
	l, r := len(left), len(right)
	return l+r <= 7 && (l != 7 || r != 0) && (l != 0 || r != 7)
}

// allComponents reports whether every group is a valid IPv6 component.
func allComponents(groups []string) bool {
	for _, g := range groups {
		if !ipv6Component(g) {
			return false
		}
	}
	return true
}

// ipv6WithIPv4 matches the IPv4-in-IPv6 forms Ansible's pattern names: (0:){6} followed by an IPv4
// address, ::ffff: or :: followed by one, and (0:){5}ffff: followed by one, where each IPv4 part
// is one to four hex digits or a hexadecimal range, as the pattern reuses the IPv6 component.
func ipv6WithIPv4(s string) bool {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return false
	}
	prefix, v4 := strings.ToLower(s[:idx+1]), s[idx+1:]
	parts := strings.Split(v4, ".")
	if len(parts) != 4 || !allComponents(parts) {
		return false
	}
	switch prefix {
	case "0:0:0:0:0:0:", "::ffff:", "::", "0:0:0:0:0:ffff:":
		return true
	}
	return false
}

// isHostnamePattern is Ansible's hostname pattern: dot-separated labels of word characters,
// hyphens, and alphanumeric ranges, each beginning with a word character or a range and not ending
// with a hyphen or an underscore.
func isHostnamePattern(s string) bool {
	if s == "" {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !hostnameLabel(label) {
			return false
		}
	}
	return true
}

// hostnameLabel matches one label of a hostname pattern.
func hostnameLabel(label string) bool {
	if label == "" {
		return false
	}
	i := 0
	first := true
	last := byte(0)
	for i < len(label) {
		if n := alphanumericRange(label[i:]); n > 0 {
			i += n
			last = ']'
			first = false
			continue
		}
		r, size := utf8.DecodeRuneInString(label[i:])
		switch {
		case pyIsWord(r):
		case r == '-' && !first:
		default:
			return false
		}
		last = label[i+size-1]
		i += size
		first = false
	}
	return last != '_' && last != '-'
}

// expandHostnameRange is Ansible's expand_hostname_range. It replaces the first [ and the first ]
// with a separator and splits there, so exactly three parts must result, expands the bounds in the
// middle part, and recurses on each result while a [ remains. A step that is not positive, which
// Ansible turns into an empty or a descending sequence, is refused rather than imitated.
func expandHostnameRange(line string, limit int) ([]string, error) {
	marked := strings.Replace(strings.Replace(line, "[", "|", 1), "]", "|", 1)
	parts := strings.Split(marked, "|")
	if len(parts) != 3 {
		return nil, fmt.Errorf("host range %q does not split into a name and one range", line)
	}
	head, nrange, tail := parts[0], parts[1], parts[2]
	bounds := strings.Split(nrange, ":")
	if len(bounds) != 2 && len(bounds) != 3 {
		return nil, fmt.Errorf("host range must be begin:end or begin:end:step")
	}
	beg, end, step := bounds[0], bounds[1], "1"
	if len(bounds) == 3 {
		step = bounds[2]
	}
	if beg == "" {
		beg = "0"
	}
	if end == "" {
		return nil, fmt.Errorf("host range must specify end value")
	}
	width := 0
	if beg[0] == '0' && len(beg) > 1 {
		width = len(beg)
		if width != len(end) {
			return nil, fmt.Errorf("host range must specify equal-length begin and end formats")
		}
	}
	stepN, ok := pyIntText(step)
	if !ok {
		return nil, fmt.Errorf("host range step %q is not an integer", step)
	}
	if stepN == 0 {
		return nil, fmt.Errorf("host range %q has a step of zero", line)
	}
	if stepN < 0 {
		return nil, needsAnsible(fmt.Sprintf("host range %q has a negative step", line))
	}
	var seq []string
	ib, ie := strings.Index(asciiLetters, beg), strings.Index(asciiLetters, end)
	if ib >= 0 && ie >= 0 {
		if ib > ie {
			return nil, fmt.Errorf("host range must have begin <= end")
		}
		for k := int64(ib); k <= int64(ie); k += stepN {
			seq = append(seq, asciiLetters[k:k+1])
		}
	} else {
		b, okB := pyIntText(beg)
		e, okE := pyIntText(end)
		if !okB || !okE {
			return nil, fmt.Errorf("host range bounds %q and %q are not integers", beg, end)
		}
		if e >= b && (e-b)/stepN+1 > int64(limit) {
			return nil, ErrTooManyHosts
		}
		for k := b; k <= e; k += stepN {
			v := strconv.FormatInt(k, 10)
			if width > 0 {
				v = pyZfill(v, width)
			}
			seq = append(seq, v)
		}
	}
	var out []string
	for _, v := range seq {
		name := head + v + tail
		if !strings.Contains(name, "[") {
			out = append(out, name)
		} else {
			more, err := expandHostnameRange(name, limit-len(out))
			if err != nil {
				return nil, err
			}
			out = append(out, more...)
		}
		if len(out) > limit {
			return nil, ErrTooManyHosts
		}
	}
	return out, nil
}

// pyIntText parses s the way Python's int() parses text, for the values a host range carries:
// optional surrounding whitespace, an optional sign, and ASCII digits with single underscores
// between them. Anything else, including digits from other scripts that Python would accept, is
// refused, which leaves those ranges to Ansible.
func pyIntText(s string) (int64, bool) {
	s = pyStrip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	digits := strings.ReplaceAll(s, "_", "")
	if !allDigits(digits) || len(digits) > 18 {
		return 0, false
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// pyZfill pads s with zeros to width the way Python's str.zfill does, keeping a leading sign first.
func pyZfill(s string, width int) string {
	if len(s) >= width {
		return s
	}
	sign := ""
	if s != "" && (s[0] == '-' || s[0] == '+') {
		sign, s = s[:1], s[1:]
	}
	return sign + strings.Repeat("0", width-len(s)-len(sign)) + s
}
