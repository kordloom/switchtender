// Package util holds the small cross-package string helpers, one implementation instead of a copy
// per package.
package util

import (
	"strings"
	"unicode/utf8"
)

// secretKeyStems are the substrings that mark a variable or field name as secret-bearing. They
// match anywhere in the name, so ansible_become_password, secret_value, and token_id are all
// caught, not only the bare names.
var secretKeyStems = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey", "api_key",
	"private_key", "privatekey",
	// An Authorization header's value is the credential itself, and it is written as an ordinary
	// name and value on a curl line, in an inventory, and in a playbook variable alike. Without this
	// stem a bearer token sat in the clear everywhere this classifier is consulted.
	"authorization",
}

// SecretKey reports whether the value stored under a key is secret material. It matches the stems
// above anywhere in the key, the exact field "fields" (the secret bag of a custom credential type),
// and the bare pass stem only as a whole key or a terminal _pass, so ansible_ssh_pass matches while
// bypass and passthrough, whose values are ordinary, do not.
//
// This is the single classifier for secret-bearing names. The audit chain uses it to keep a secret
// out of the digest it publishes, and the inventory redactor uses it to keep one out of the content
// it serves and to hand it to the run-log masker. Two classifiers would drift, and a name only one
// of them recognized would be redacted in one place and served in the other.
func SecretKey(key string) bool {
	k := strings.ToLower(key)
	if k == "fields" || k == "pass" || strings.HasSuffix(k, "_pass") {
		return true
	}
	for _, stem := range secretKeyStems {
		if strings.Contains(k, stem) {
			return true
		}
	}
	return false
}

// FirstNonEmpty returns the first value that is not empty, or empty when all are.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Clip shortens s to at most limit bytes without splitting a UTF-8 rune, appending an ellipsis when
// the value was cut.
func Clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
