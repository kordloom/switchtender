package inventory

import "regexp"

// secretPattern matches an inventory variable assignment whose name suggests a secret, in the ini
// form key=value or the yaml form key: value. The captured group is the value, which is either a
// quoted run (spaces allowed, up to the closing quote) or an unquoted run up to the next whitespace.
// The quoted branch is what lets a password with spaces be matched in full; the unquoted branch
// stops at whitespace so a second variable on the same INI host line is still matched on the next
// pass rather than swallowed. The password, passwd, passphrase, secret, token, and api_key stems may
// carry trailing name parts, so secret_value and token_id still match. The bare pass stem is
// deliberately narrower: it matches only as a terminal _pass component, which catches ansible_ssh_pass
// and ansible_become_pass without swallowing bypass, passive, or passthrough, whose values are often
// booleans that would then be blacked out everywhere. Key-file paths and access-key IDs are not
// secret and are left unmatched.
var secretPattern = regexp.MustCompile(
	`(?i)[a-z0-9_]*(?:(?:password|passwd|passphrase|secret|token|api[_-]?key)[a-z0-9_]*|_pass)` +
		`\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s]+)`)

// RedactedValue stands in for a secret value removed from inventory content.
const RedactedValue = "***"

// Secrets returns the values of secret-looking variables in inventory content, so a host list that
// carries an ansible_password or an API token does not leak it into a run's log or events. A quoted
// value is unwrapped so a masker holds the bare secret and matches it literally in output; capturing
// the surrounding quotes would mask a string the output never contains.
func Secrets(content string) []string {
	var out []string
	for _, m := range secretPattern.FindAllStringSubmatch(content, -1) {
		v := m[1]
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Redact replaces the value of every secret-looking variable in content, keeping the variable names
// and the host layout so the inventory still reads as one.
//
// An inventory is served to anyone who may read it, and a host list routinely carries an
// ansible_password or a become password inline. Those are credentials, and the rest of the API does
// not hand a credential's material to a reader, so this one should not either. The names stay
// because knowing that a host sets ansible_password is not the secret; its value is.
func Redact(content string) string {
	return secretPattern.ReplaceAllStringFunc(content, func(m string) string {
		loc := secretPattern.FindStringSubmatchIndex(m)
		if loc == nil || loc[2] < 0 {
			return m
		}
		return m[:loc[2]] + RedactedValue
	})
}
