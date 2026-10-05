package inventory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// maxHostFilterLen bounds a host filter so a pasted or imported document cannot make parsing a cost
// of its own. AWX keeps the field in a 1024 character column, so this admits anything AWX does.
const maxHostFilterLen = 4096

// maxHostFilterDepth bounds how deeply parentheses and not may nest, so a hostile filter cannot
// exhaust the stack of the recursive parser.
const maxHostFilterDepth = 64

// Candidate is one host a smart inventory weighs, with everything a host filter can ask about it.
type Candidate struct {
	// Name is the host's inventory name.
	Name string
	// Groups are the groups the host is a direct member of in its inventory, without all and
	// ungrouped. AWX matches groups__name on direct membership, and so does this.
	Groups []string
	// Vars are the host's variables as ansible-inventory resolves them, group variables included.
	Vars map[string]any
	// Facts are the system facts last gathered for the host, nil when none ever were.
	Facts map[string]string
	// InventoryID is the id of the inventory the host came from.
	InventoryID string
	// InventoryName is the name of the inventory the host came from.
	InventoryName string
}

// HostFilter is a parsed smart inventory host filter.
//
// The syntax is AWX's host_filter: terms written key=value, joined by and, or, and not, grouped by
// parentheses, with two terms side by side meaning and. A key is a field and, after a double
// underscore, a lookup, the way Django spells one: name__icontains=web. The fields are name,
// groups__name, inventory, inventory__name, enabled, search, variables, and ansible_facts.
type HostFilter struct {
	// root is the parsed expression.
	root filterNode
	// facts reports whether any term reads ansible_facts, so a caller only loads facts when asked.
	facts bool
}

// UsesFacts reports whether the filter asks about gathered facts.
func (f *HostFilter) UsesFacts() bool {
	return f != nil && f.facts
}

// Match reports whether the filter selects the host.
func (f *HostFilter) Match(c *Candidate) bool {
	if f == nil || f.root == nil || c == nil {
		return false
	}
	return f.root.match(c)
}

// filterNode is one node of a parsed host filter.
type filterNode interface {
	// match reports whether the node selects the host.
	match(c *Candidate) bool
}

// andNode selects a host every child selects.
type andNode struct {
	// kids are the conjoined expressions.
	kids []filterNode
}

// match reports whether every child selects the host.
func (n andNode) match(c *Candidate) bool {
	for _, k := range n.kids {
		if !k.match(c) {
			return false
		}
	}
	return true
}

// orNode selects a host any child selects.
type orNode struct {
	// kids are the disjoined expressions.
	kids []filterNode
}

// match reports whether any child selects the host.
func (n orNode) match(c *Candidate) bool {
	for _, k := range n.kids {
		if k.match(c) {
			return true
		}
	}
	return false
}

// notNode selects a host its child does not.
type notNode struct {
	// kid is the negated expression.
	kid filterNode
}

// match reports whether the child does not select the host.
func (n notNode) match(c *Candidate) bool {
	return !n.kid.match(c)
}

// termNode is one key=value comparison.
type termNode struct {
	// field is the host attribute the term reads: name, groups, inventory, inventory_name,
	// enabled, search, variables, or facts.
	field string
	// path is the key path under variables or ansible_facts, empty for the other fields.
	path []string
	// cmp compares one value the field yields against the term's value.
	cmp comparison
}

// match reports whether the term selects the host.
func (t termNode) match(c *Candidate) bool {
	switch t.field {
	case "name":
		return t.cmp.test(c.Name, true)
	case "search":
		return t.cmp.test(c.Name, true)
	case "groups":
		return slices.ContainsFunc(c.Groups, func(g string) bool { return t.cmp.test(g, true) })
	case "inventory":
		return t.cmp.test(c.InventoryID, true)
	case "inventory_name":
		return t.cmp.test(c.InventoryName, true)
	case "enabled":
		// Every host a stored inventory holds is enabled: the importer leaves out the hosts AWX had
		// switched off, and INI has no off switch for one. So enabled=true selects every host and
		// enabled=false selects none, which is what each means in AWX for the hosts that came across.
		return t.cmp.test("true", true)
	case "variables":
		if len(t.path) == 0 {
			return t.cmp.test(varsText(c.Vars), true)
		}
		return matchPath(toAny(c.Vars), t.path, t.cmp)
	case "facts":
		if c.Facts == nil {
			return false
		}
		return matchPath(factDoc(c.Facts), t.path, t.cmp)
	}
	return false
}

// comparison is a Django-style lookup against one value.
type comparison struct {
	// lookup is the comparison: exact, iexact, contains, icontains, startswith, istartswith,
	// endswith, iendswith, regex, iregex, gt, gte, lt, lte, in, or isnull.
	lookup string
	// value is the term's value as written.
	value string
	// re is the compiled pattern for regex and iregex.
	re *regexp.Regexp
	// set is the value split on commas for in.
	set []string
}

// test compares v, the host's value, against the term. present is false when the host has no such
// value, which only isnull can match.
func (c comparison) test(v string, present bool) bool {
	if c.lookup == "isnull" {
		want := strings.EqualFold(c.value, "true")
		return want != present
	}
	if !present {
		return false
	}
	switch c.lookup {
	case "exact":
		return v == c.value || (isBoolText(c.value) && strings.EqualFold(v, c.value))
	case "iexact":
		return strings.EqualFold(v, c.value)
	case "contains":
		return strings.Contains(v, c.value)
	case "icontains":
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.value))
	case "startswith":
		return strings.HasPrefix(v, c.value)
	case "istartswith":
		return strings.HasPrefix(strings.ToLower(v), strings.ToLower(c.value))
	case "endswith":
		return strings.HasSuffix(v, c.value)
	case "iendswith":
		return strings.HasSuffix(strings.ToLower(v), strings.ToLower(c.value))
	case "regex", "iregex":
		return c.re.MatchString(v)
	case "in":
		return slices.Contains(c.set, v)
	case "gt", "gte", "lt", "lte":
		return ordered(c.lookup, v, c.value)
	}
	return false
}

// isBoolText reports whether s spells a boolean, which AWX reads as one for a fact.
func isBoolText(s string) bool {
	return strings.EqualFold(s, "true") || strings.EqualFold(s, "false")
}

// ordered compares v against want under an ordering lookup, numerically when both read as numbers
// and as text otherwise.
func ordered(lookup, v, want string) bool {
	var cmp int
	a, aerr := strconv.ParseFloat(v, 64)
	b, berr := strconv.ParseFloat(want, 64)
	if aerr == nil && berr == nil {
		switch {
		case a < b:
			cmp = -1
		case a > b:
			cmp = 1
		}
	} else {
		cmp = strings.Compare(v, want)
	}
	switch lookup {
	case "gt":
		return cmp > 0
	case "gte":
		return cmp >= 0
	case "lt":
		return cmp < 0
	default:
		return cmp <= 0
	}
}

// lookups are the comparisons a term may name after its field.
var lookups = []string{
	"exact", "iexact", "contains", "icontains", "startswith", "istartswith", "endswith",
	"iendswith", "regex", "iregex", "gt", "gte", "lt", "lte", "in", "isnull",
}

// matchPath walks a decoded value along path and reports whether the comparison holds for what it
// reaches. A segment ending in [] reads a list and matches when any element does, which is how AWX
// spells a fact held in a list.
func matchPath(doc any, path []string, cmp comparison) bool {
	if len(path) == 0 {
		s, ok := scalarText(doc)
		return cmp.test(s, ok)
	}
	seg := path[0]
	list := strings.HasSuffix(seg, "[]")
	key := strings.TrimSuffix(seg, "[]")
	m, ok := doc.(map[string]any)
	if !ok {
		return cmp.test("", false)
	}
	child, has := m[key]
	if !has {
		return cmp.test("", false)
	}
	if !list {
		return matchPath(child, path[1:], cmp)
	}
	items, ok := child.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if matchPath(item, path[1:], cmp) {
			return true
		}
	}
	return false
}

// scalarText renders a decoded leaf the way a filter value is written, reporting false for a value
// that is absent or null.
func scalarText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// varsText renders a host's variables as one text, which is what AWX compares a bare variables
// lookup against, since it keeps a host's variables as a text field.
func varsText(vars map[string]any) string {
	if len(vars) == 0 {
		return ""
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return ""
	}
	return string(b)
}

// toAny widens a variable map to the any a path walk reads, keeping nil as an absent document.
func toAny(vars map[string]any) any {
	if vars == nil {
		return nil
	}
	return vars
}

// factDoc lays the facts SwitchTender keeps out the way AWX addresses them.
//
// The fact store keeps a short list of facts by their name without the ansible_ prefix, and the
// default route as ip. AWX filters address them with the prefix,
// ansible_facts__ansible_distribution, and the route as ansible_default_ipv4__address, so both
// spellings are offered and either matches.
func factDoc(facts map[string]string) map[string]any {
	doc := make(map[string]any, 2*len(facts)+2)
	for k, v := range facts {
		doc[k] = v
		doc["ansible_"+k] = v
	}
	if ip, ok := facts["ip"]; ok {
		route := map[string]any{"address": ip}
		doc["default_ipv4"] = route
		doc["ansible_default_ipv4"] = route
	}
	return doc
}

// ParseHostFilter reads an AWX host_filter. It refuses a filter naming a field or lookup it does
// not know rather than reading it as matching nothing, because a smart inventory that silently
// selects no host looks exactly like one whose hosts all went away.
func ParseHostFilter(s string) (*HostFilter, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrHostFilter)
	}
	if len(s) > maxHostFilterLen {
		return nil, fmt.Errorf("%w: it is longer than %d characters", ErrHostFilter, maxHostFilterLen)
	}
	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	p := &filterParser{toks: toks}
	root, err := p.or(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("%w: unexpected %s", ErrHostFilter, p.toks[p.pos])
	}
	return &HostFilter{root: root, facts: p.facts}, nil
}

// token is one lexical element of a host filter.
type token struct {
	// kind is lparen, rparen, word, or term.
	kind string
	// key is a term's key, or a word's text.
	key string
	// value is a term's value.
	value string
}

// String renders the token for an error message.
func (t token) String() string {
	switch t.kind {
	case "lparen":
		return `"("`
	case "rparen":
		return `")"`
	case "term":
		return strconv.Quote(t.key + "=" + t.value)
	}
	return strconv.Quote(t.key)
}

// keyChar reports whether r may appear in a term's key.
func keyChar(r byte) bool {
	return r == '_' || r == '.' || r == '[' || r == ']' || r == '-' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// tokenize splits a host filter into parentheses, the words and, or, and not, and key=value terms.
func tokenize(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
			continue
		case c == '(':
			out = append(out, token{kind: "lparen"})
			i++
			continue
		case c == ')':
			out = append(out, token{kind: "rparen"})
			i++
			continue
		}
		start := i
		for i < len(s) && keyChar(s[i]) {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("%w: unexpected %q", ErrHostFilter, string(c))
		}
		key := s[start:i]
		if i >= len(s) || s[i] != '=' {
			out = append(out, token{kind: "word", key: key})
			continue
		}
		i++
		value, next, err := readValue(s, i)
		if err != nil {
			return nil, err
		}
		i = next
		out = append(out, token{kind: "term", key: key, value: value})
	}
	return out, nil
}

// readValue reads a term's value starting at i, quoted or bare, and returns it with the position
// after it. A bare value runs to the next space or closing parenthesis.
func readValue(s string, i int) (string, int, error) {
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		quote := s[i]
		var b strings.Builder
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				if j+1 < len(s) {
					j++
					b.WriteByte(s[j])
				}
			case quote:
				return b.String(), j + 1, nil
			default:
				b.WriteByte(s[j])
			}
		}
		return "", 0, fmt.Errorf("%w: a quoted value is not closed", ErrHostFilter)
	}
	start := i
	for i < len(s) && !unicode.IsSpace(rune(s[i])) && s[i] != ')' && s[i] != '(' {
		i++
	}
	if i == start {
		return "", 0, fmt.Errorf("%w: a term has no value", ErrHostFilter)
	}
	return s[start:i], i, nil
}

// filterParser is a recursive descent parser over host filter tokens.
type filterParser struct {
	// toks are the tokens.
	toks []token
	// pos is the next token to read.
	pos int
	// facts records that a term read ansible_facts.
	facts bool
}

// peek returns the next token and whether there is one.
func (p *filterParser) peek() (token, bool) {
	if p.pos >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.pos], true
}

// isWord reports whether t is the keyword w, in any case.
func isWord(t token, w string) bool {
	return t.kind == "word" && strings.EqualFold(t.key, w)
}

// or parses a disjunction, the loosest binding.
func (p *filterParser) or(depth int) (filterNode, error) {
	first, err := p.and(depth)
	if err != nil {
		return nil, err
	}
	kids := []filterNode{first}
	for {
		t, ok := p.peek()
		if !ok || !isWord(t, "or") {
			break
		}
		p.pos++
		next, err := p.and(depth)
		if err != nil {
			return nil, err
		}
		kids = append(kids, next)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return orNode{kids: kids}, nil
}

// and parses a conjunction, written with and or by placing two expressions side by side.
func (p *filterParser) and(depth int) (filterNode, error) {
	first, err := p.unary(depth)
	if err != nil {
		return nil, err
	}
	kids := []filterNode{first}
	for {
		t, ok := p.peek()
		if !ok || t.kind == "rparen" || isWord(t, "or") {
			break
		}
		if isWord(t, "and") {
			p.pos++
		}
		next, err := p.unary(depth)
		if err != nil {
			return nil, err
		}
		kids = append(kids, next)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return andNode{kids: kids}, nil
}

// unary parses a negation, a parenthesized expression, or a term.
func (p *filterParser) unary(depth int) (filterNode, error) {
	if depth > maxHostFilterDepth {
		return nil, fmt.Errorf("%w: it nests deeper than %d", ErrHostFilter, maxHostFilterDepth)
	}
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("%w: it ends where a term was expected", ErrHostFilter)
	}
	switch {
	case isWord(t, "not"):
		p.pos++
		kid, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return notNode{kid: kid}, nil
	case t.kind == "lparen":
		p.pos++
		inner, err := p.or(depth + 1)
		if err != nil {
			return nil, err
		}
		closing, ok := p.peek()
		if !ok || closing.kind != "rparen" {
			return nil, fmt.Errorf("%w: a parenthesis is not closed", ErrHostFilter)
		}
		p.pos++
		return inner, nil
	case t.kind == "term":
		p.pos++
		return p.term(t)
	}
	return nil, fmt.Errorf("%w: expected a key=value term at %s", ErrHostFilter, t)
}

// term builds a comparison from one key=value pair.
func (p *filterParser) term(t token) (filterNode, error) {
	segs := strings.Split(t.key, "__")
	if slices.Contains(segs, "") {
		return nil, fmt.Errorf("%w: %q is not a field", ErrHostFilter, t.key)
	}
	// splitLookup takes a trailing lookup off rest, defaulting to an exact match.
	splitLookup := func(rest []string) ([]string, string) {
		if n := len(rest); n > 0 && slices.Contains(lookups, rest[n-1]) {
			return rest[:n-1], rest[n-1]
		}
		return rest, "exact"
	}
	node := termNode{}
	var lookup string
	var rest []string
	switch segs[0] {
	case "name", "search", "enabled":
		node.field = segs[0]
		rest, lookup = splitLookup(segs[1:])
		if segs[0] == "search" && lookup == "exact" {
			lookup = "icontains"
		}
	case "groups":
		if len(segs) < 2 || segs[1] != "name" {
			return nil, fmt.Errorf("%w: groups is matched by name, as groups__name", ErrHostFilter)
		}
		node.field = "groups"
		rest, lookup = splitLookup(segs[2:])
	case "inventory":
		node.field = "inventory"
		rest, lookup = splitLookup(segs[1:])
		if len(rest) == 1 && (rest[0] == "id" || rest[0] == "name") {
			if rest[0] == "name" {
				node.field = "inventory_name"
			}
			rest = nil
		}
	case "variables":
		node.field = "variables"
		node.path, lookup = splitLookup(segs[1:])
	case "ansible_facts":
		// AWX compares a fact by containment, so it takes no lookup. The value is read as written.
		node.field = "facts"
		node.path, lookup = segs[1:], "exact"
		if len(node.path) == 0 {
			return nil, fmt.Errorf("%w: ansible_facts needs a fact name, as "+
				"ansible_facts__ansible_distribution", ErrHostFilter)
		}
		p.facts = true
	default:
		return nil, fmt.Errorf("%w: %q is not a field a host filter can read; use name, "+
			"groups__name, inventory, inventory__name, enabled, search, variables, or ansible_facts",
			ErrHostFilter, segs[0])
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("%w: %q is not a lookup", ErrHostFilter, strings.Join(rest, "__"))
	}
	if node.field == "enabled" && !isBoolText(t.value) {
		return nil, fmt.Errorf("%w: enabled takes true or false", ErrHostFilter)
	}
	node.cmp = comparison{lookup: lookup, value: t.value}
	switch lookup {
	case "regex", "iregex":
		pattern := t.value
		if lookup == "iregex" {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a valid pattern: %w", ErrHostFilter, t.value, err)
		}
		node.cmp.re = re
	case "in":
		for _, v := range strings.Split(t.value, ",") {
			node.cmp.set = append(node.cmp.set, strings.TrimSpace(v))
		}
	case "isnull":
		if !isBoolText(t.value) {
			return nil, fmt.Errorf("%w: isnull takes true or false", ErrHostFilter)
		}
	}
	return node, nil
}
