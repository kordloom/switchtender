package util

// RedactDeep returns a deep copy of value with every secret removed at any depth, and reports
// whether anything was redacted so a caller can skip a copy it does not need.
//
// It walks maps and slices rather than inspecting only the leaves it happens to reach first. A
// scrubber that looked at top-level string values alone let one level of nesting carry a secret
// straight through: extra_vars of {"db":{"password":"..."}} is a map, not a string, so it was
// skipped entirely, and so was a secret-named key holding an array or a number. The key names a
// secret whatever shape its value has, which is why the key is checked before the value's type is
// considered at all.
//
// The copy is deep on purpose. A shallow one hands the caller a new outer map whose inner maps are
// still the caller's, so writing a redaction into a nested map would edit the live record the
// scrubber was protecting.
func RedactDeep(value any, marker string) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		changed := false
		for key, child := range v {
			// The key alone can name a secret, as it does for -e db_password=x, and then the whole
			// value is the secret rather than an assignment inside it, whatever type it is.
			if SecretKey(key) {
				out[key], changed = marker, true
				continue
			}
			red, hit := RedactDeep(child, marker)
			out[key] = red
			changed = changed || hit
		}
		return out, changed
	case []any:
		out := make([]any, len(v))
		changed := false
		for i, child := range v {
			red, hit := RedactDeep(child, marker)
			out[i] = red
			changed = changed || hit
		}
		return out, changed
	case string:
		masked, _ := RedactAssignments(v, marker)
		return masked, masked != v
	default:
		// Numbers, booleans, and nil carry no assignment to find and are returned as they are. A
		// secret held in one is caught by its key above.
		return v, false
	}
}
