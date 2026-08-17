package sqlutil

import "strings"

// SchemaColumn is one column as a CREATE TABLE statement declares it: the name, and everything after
// the name, which is exactly what an ALTER TABLE ADD COLUMN needs.
type SchemaColumn struct {
	// Name is the column name.
	Name string
	// Clause is the column's type, constraints, and default, verbatim from the schema.
	Clause string
}

// ParseSchemaColumns reads the CREATE TABLE statements out of a schema blob and returns each table's
// columns in declaration order.
//
// It exists because migrations were a hand-maintained list of ALTER statements beside a schema that
// kept growing, and the two drifted: a column added to the CREATE and to the shared select list but
// not to the list broke every read of its table on any database created before it. Deriving the
// migration from the schema itself removes the list there was to forget.
//
// The parser handles the shape these schemas are actually written in: one column per line, table
// constraints on their own lines, SQL comments starting with --. It is not a SQL parser and does not
// try to be; the schema is our own file, held to our own layout, and the tests that compare a healed
// database against a fresh one are what keep this honest.
func ParseSchemaColumns(schema string) map[string][]SchemaColumn {
	out := map[string][]SchemaColumn{}
	// Comments go first, before the marker scan ever runs. The pg schema contains a comment that
	// names the marker phrase, and matching it produced a phantom table whose key was the comment's
	// tail: harmless by coincidence, one edit away from either failing every open with a junk ALTER
	// or silently stealing a real table's columns onto an unhealable key.
	rest := stripLineComments(schema)
	const marker = "CREATE TABLE IF NOT EXISTS "
	for {
		at := indexAtLineStart(rest, marker)
		if at == -1 {
			return out
		}
		rest = rest[at+len(marker):]
		open := strings.IndexByte(rest, '(')
		if open == -1 {
			return out
		}
		table := strings.TrimSpace(rest[:open])
		body, after, found := cutBalanced(rest[open:])
		if !found {
			return out
		}
		rest = after
		// A table name that is not a plain identifier means the scan latched onto something that is
		// not a CREATE statement. Skipping it silently would hide a parse gone wrong, and these
		// parsed names are spliced into ALTER statements, so nothing that is not an identifier may
		// pass this point.
		if !isIdentifier(table) {
			continue
		}
		out[table] = parseColumnLines(body)
	}
}

// stripLineComments removes -- comments, each to its end of line, so nothing inside one can be
// mistaken for SQL.
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "--"); at != -1 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// indexAtLineStart returns the index of marker where it begins a statement: at the start of a line,
// allowing leading whitespace. A marker phrase in the middle of a line is prose, not SQL.
func indexAtLineStart(s, marker string) int {
	from := 0
	for {
		at := strings.Index(s[from:], marker)
		if at == -1 {
			return -1
		}
		at += from
		lineStart := strings.LastIndexByte(s[:at], '\n') + 1
		if strings.TrimSpace(s[lineStart:at]) == "" {
			return at
		}
		from = at + len(marker)
	}
}

// isIdentifier reports whether s is a plain SQL identifier: letters, digits, and underscores,
// starting with a letter or underscore.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !alpha && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// cutBalanced returns the text inside the first balanced parenthesis pair, and what follows it. The
// body itself contains parentheses, in defaults and in table constraints, so a plain index of the
// closing brace cuts the body short.
func cutBalanced(s string) (body, after string, found bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// constraintPrefixes are the table-level lines a CREATE body holds that are not columns.
var constraintPrefixes = []string{"PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK", "CONSTRAINT"}

// parseColumnLines reads one CREATE body into columns, skipping constraint lines and comments. A
// comment carries no trailing comma, so it arrives fused to the column after it in one comma-split
// segment; the comment lines are stripped from the segment rather than the segment discarded, or the
// column following a comment would silently vanish from every heal.
func parseColumnLines(body string) []SchemaColumn {
	var out []SchemaColumn
	for _, segment := range splitTopLevel(body) {
		var kept []string
		for _, raw := range strings.Split(segment, "\n") {
			if trimmed := strings.TrimSpace(raw); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
				kept = append(kept, trimmed)
			}
		}
		line := strings.TrimSpace(strings.Join(kept, " "))
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		var constraint bool
		for _, p := range constraintPrefixes {
			// The keyword has to stand alone: a column named checksum starts with CHECK, and a column
			// named unique_key starts with UNIQUE, and treating either as a table constraint would
			// silently drop it from every heal, which is the org_id incident again through the
			// mechanism built to end it.
			if upper == p || strings.HasPrefix(upper, p+" ") || strings.HasPrefix(upper, p+"(") {
				constraint = true
				break
			}
		}
		if constraint {
			continue
		}
		name, clause, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out = append(out, SchemaColumn{Name: name, Clause: strings.Join(strings.Fields(clause), " ")})
	}
	return out
}

// splitTopLevel splits a CREATE body on the commas between columns, leaving commas inside
// parentheses, such as a composite primary key's member list, where they are.
func splitTopLevel(body string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	return append(out, body[start:])
}

// Addable reports whether the column can be added to an existing table by ALTER TABLE. A NOT NULL
// column with no default cannot be, and a primary key member never can. Both kinds are original-era
// columns by construction: a table cannot have been created without its key, and every column added
// after a table shipped carries a default precisely so old rows have a value.
func (c SchemaColumn) Addable() bool {
	upper := strings.ToUpper(c.Clause)
	if strings.Contains(upper, "PRIMARY KEY") {
		return false
	}
	if strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "DEFAULT") {
		return false
	}
	return true
}
