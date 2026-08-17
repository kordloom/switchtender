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
	rest := schema
	const marker = "CREATE TABLE IF NOT EXISTS "
	for {
		at := strings.Index(rest, marker)
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
		out[table] = parseColumnLines(body)
	}
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
			if strings.HasPrefix(upper, p) {
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
