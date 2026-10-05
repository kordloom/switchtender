package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is Ansible's YAML inventory plugin and the loader under it. Ansible reads the file as
// JSON first and as YAML only when that fails, and reads YAML with PyYAML's YAML 1.1 rules, which
// differ from YAML 1.2 in ways an inventory meets: yes, no, on, and off are booleans, 010 is octal,
// 1:20 is the base 60 number 80, and 1e3 is a string while 1.0e+3 is a float. Every scalar here is
// resolved by those rules rather than by the YAML library's own.

// errNotYAMLInventory reports that the YAML plugin declines the document, as Ansible's does when
// the file does not load or does not hold a mapping, so the INI plugin gets its turn.
var errNotYAMLInventory = errors.New("not a YAML inventory")

// loadInventoryDocument loads content the way Ansible's DataLoader does for the YAML plugin: as
// JSON when Python's json module accepts it, and as a single YAML document otherwise. It returns
// errNotYAMLInventory when neither loads.
func loadInventoryDocument(content string) (any, error) {
	if v, ok, err := loadJSON(content); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	return loadYAML(content)
}

// loadJSON parses content as Python's json.loads does, reporting false when it is not JSON. Objects
// keep their keys in order, numbers keep their literal text, and an object holding __ansible_unsafe
// is read back as the unsafe string it stands for, as Ansible's JSON decoder reads it.
func loadJSON(content string) (any, bool, error) {
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	v, err := jsonValue(dec, 0)
	if err != nil {
		if jsonSpecialFloat.MatchString(content) {
			return nil, false, needsAnsible("the JSON holds NaN or Infinity")
		}
		return nil, false, nil
	}
	rest := content[dec.InputOffset():]
	if strings.Trim(rest, " \t\n\r") != "" {
		return nil, false, nil
	}
	if err := checkSurrogates(content); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// jsonSpecialFloat finds the NaN and Infinity literals Python's json module accepts and JSON does
// not.
var jsonSpecialFloat = regexp.MustCompile(`[:\[,]\s*-?(?:NaN|Infinity)\b`)

// jsonValue decodes one JSON value from dec into the engine's value types.
func jsonValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxLiteralDepth {
		return nil, needsAnsible("the JSON nests too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := newOrderedMap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := jsonValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				m.Set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return jsonTagged(m)
		case '[':
			out := []any{}
			for dec.More() {
				v, err := jsonValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
	case string:
		return t, nil
	case bool:
		return t, nil
	case nil:
		return nil, nil
	case json.Number:
		return jsonNumber(string(t))
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

// jsonNumber converts a JSON number the way Python's json module does: an int when it has no
// fraction or exponent, a float otherwise.
func jsonNumber(text string) (any, error) {
	if !strings.ContainsAny(text, ".eE") {
		sign := 1
		digits := text
		if strings.HasPrefix(digits, "-") {
			sign, digits = -1, digits[1:]
		}
		n, ok := pyInt(sign, digits, 10)
		if !ok {
			return nil, needsAnsible("the JSON holds an integer longer than Python converts")
		}
		return n, nil
	}
	f, ok := pyFloatValue(text)
	if !ok {
		return nil, needsAnsible("the JSON holds a float too large to carry")
	}
	n, _ := pyFloat(f)
	return n, nil
}

// jsonTagged reads an object the way Ansible's JSON decoder does: one carrying __ansible_unsafe is
// the unsafe string under it, and one carrying another of Ansible's type keys is left to Ansible.
func jsonTagged(m *orderedMap) (any, error) {
	if v, ok := m.Get("__ansible_unsafe"); ok {
		s, isString := v.(string)
		if m.Len() != 1 || !isString {
			return nil, needsAnsible("an __ansible_unsafe value is not a lone string")
		}
		return unsafeString(s), nil
	}
	for _, key := range []string{"__ansible_vault", "__ansible_type"} {
		if _, ok := m.Get(key); ok {
			return nil, needsAnsible("the JSON holds a vault-encrypted or typed Ansible value")
		}
	}
	return m, nil
}

// surrogateEscape matches a \u escape of a UTF-16 surrogate in JSON text.
var surrogateEscape = regexp.MustCompile(`\\u[dD][89a-fA-F][0-9a-fA-F]{2}`)

// checkSurrogates refuses JSON whose \u escapes leave a lone surrogate, which Python keeps in a
// string and Go replaces, so the two would read different text.
func checkSurrogates(content string) error {
	locs := surrogateEscape.FindAllStringIndex(content, -1)
	for i := 0; i < len(locs); i++ {
		start := locs[i][0]
		backslashes := 0
		for k := start - 1; k >= 0 && content[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		hi, _ := strconv.ParseUint(content[start+2:start+6], 16, 32)
		if hi >= 0xd800 && hi <= 0xdbff && i+1 < len(locs) && locs[i+1][0] == locs[i][1] {
			lo, _ := strconv.ParseUint(content[locs[i+1][0]+2:locs[i+1][0]+6], 16, 32)
			if lo >= 0xdc00 && lo <= 0xdfff {
				i++
				continue
			}
		}
		return needsAnsible("the JSON holds a lone surrogate escape")
	}
	return nil
}

// loadYAML parses content as one YAML document with PyYAML's rules. It returns errNotYAMLInventory
// when the content does not parse or holds more than one document, as PyYAML's single-document load
// fails then.
func loadYAML(content string) (any, error) {
	dec := yaml.NewDecoder(strings.NewReader(content))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %w", errNotYAMLInventory, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: it holds more than one document", errNotYAMLInventory)
	}
	b := &yamlBuilder{building: map[*yaml.Node]bool{}, maxNodes: aliasNodeBudget(len(content))}
	return b.value(&doc, false)
}

// Bounds on how far a YAML document's aliases may expand. An alias is the one construct that turns a
// short document into an unbounded number of values: each reference to an anchor materializes the
// whole subtree again, so a handful of ten-wide anchors stacked a few deep expand to 10^depth
// values. Real ansible-inventory and PyYAML do not bound this and hang on such a document, so the
// native engine refuses it rather than deferring to a tool that would hang too.
const (
	// aliasNodeFanout is how many constructed values are allowed per byte of the document. A document
	// produces at most a few values per byte without aliases, so a count far above its size can only
	// come from alias expansion. It is set well above any plain document's ratio so ordinary anchors
	// and merge keys, which a real inventory uses, stay within it.
	aliasNodeFanout = 256
	// aliasNodeFloor is the number of values allowed regardless of size, so a small document with
	// modest aliasing is never refused.
	aliasNodeFloor = 1 << 16
)

// aliasNodeBudget returns how many values a document of contentLen bytes may construct before it is
// refused as expanding too far through aliases.
func aliasNodeBudget(contentLen int) int {
	return contentLen*aliasNodeFanout + aliasNodeFloor
}

// yamlBuilder constructs values from YAML nodes the way PyYAML's SafeConstructor and Ansible's
// constructor do.
type yamlBuilder struct {
	// building marks the nodes under construction, so an alias that refers back to its own anchor
	// is refused rather than followed forever.
	building map[*yaml.Node]bool
	// depth is how deeply the current value nests.
	depth int
	// nodes counts the values constructed so far, across the whole document, against maxNodes.
	nodes int
	// maxNodes bounds how many values the document may construct, so alias expansion cannot run
	// without end. Zero leaves it unbounded.
	maxNodes int
}

// The YAML styles that make a scalar something other than plain.
const yamlQuotedStyles = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle |
	yaml.FoldedStyle

// value constructs n. unsafe marks a value under the !unsafe tag.
func (b *yamlBuilder) value(n *yaml.Node, unsafe bool) (any, error) {
	if b.nodes++; b.maxNodes > 0 && b.nodes > b.maxNodes {
		return nil, errAliasExpansion
	}
	if b.depth++; b.depth > maxLiteralDepth {
		return nil, needsAnsible("the YAML nests too deeply")
	}
	defer func() { b.depth-- }()
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return b.value(n.Content[0], unsafe)
	case yaml.AliasNode:
		if b.building[n.Alias] {
			return nil, needsAnsible("the YAML holds an alias to itself")
		}
		return b.value(n.Alias, unsafe)
	case yaml.ScalarNode:
		return b.scalar(n, unsafe)
	case yaml.SequenceNode:
		if n.Style&yaml.TaggedStyle != 0 && n.ShortTag() != "!!seq" {
			return nil, needsAnsible(fmt.Sprintf("the YAML uses the %s tag", n.Tag))
		}
		b.building[n] = true
		defer delete(b.building, n)
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := b.value(c, unsafe)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		if n.Style&yaml.TaggedStyle != 0 && n.ShortTag() != "!!map" {
			return nil, needsAnsible(fmt.Sprintf("the YAML uses the %s tag", n.Tag))
		}
		return b.mapping(n, unsafe)
	}
	return nil, needsAnsible("the YAML holds a node the native engine does not read")
}

// mapping constructs a mapping, applying merge keys first as PyYAML's flatten_mapping does: the
// merged pairs come before the mapping's own, so an explicit key wins over a merged one, and of
// several merged mappings the first listed wins.
func (b *yamlBuilder) mapping(n *yaml.Node, unsafe bool) (any, error) {
	b.building[n] = true
	defer delete(b.building, n)
	pairs, err := b.flatten(n, 0)
	if err != nil {
		return nil, err
	}
	m := newOrderedMap()
	for _, p := range pairs {
		k, err := b.key(p[0])
		if err != nil {
			return nil, err
		}
		v, err := b.value(p[1], unsafe)
		if err != nil {
			return nil, err
		}
		if !m.Set(k, v) {
			return nil, needsAnsible("a mapping key is not a string or an integer")
		}
	}
	return m, nil
}

// resolveAlias returns the node an alias refers to, or n itself.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// isMergeKey reports whether a key node is the merge key <<, written plain.
func isMergeKey(k *yaml.Node) bool {
	k = resolveAlias(k)
	if k.Kind != yaml.ScalarNode {
		return false
	}
	if k.Style&yaml.TaggedStyle != 0 {
		return k.ShortTag() == "!!merge"
	}
	return k.Style&yamlQuotedStyles == 0 && k.Value == "<<"
}

// flatten returns a mapping's key and value pairs with its merge keys applied.
func (b *yamlBuilder) flatten(n *yaml.Node, depth int) ([][2]*yaml.Node, error) {
	if depth > maxLiteralDepth {
		return nil, needsAnsible("the YAML merges too deeply")
	}
	var merged, own [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMergeKey(k) {
			own = append(own, [2]*yaml.Node{k, v})
			continue
		}
		target := resolveAlias(v)
		switch target.Kind {
		case yaml.MappingNode:
			sub, err := b.flatten(target, depth+1)
			if err != nil {
				return nil, err
			}
			merged = append(merged, sub...)
		case yaml.SequenceNode:
			var parts [][][2]*yaml.Node
			for _, c := range target.Content {
				c = resolveAlias(c)
				if c.Kind != yaml.MappingNode {
					return nil, invalidf("a merge key's list holds something other than a mapping")
				}
				sub, err := b.flatten(c, depth+1)
				if err != nil {
					return nil, err
				}
				parts = append(parts, sub)
			}
			for i := len(parts) - 1; i >= 0; i-- {
				merged = append(merged, parts[i]...)
			}
		default:
			return nil, invalidf("a merge key's value is not a mapping or a list of mappings")
		}
	}
	return append(merged, own...), nil
}

// key constructs a mapping key. A plain = is the string "=" as a key, as PyYAML reads it there.
func (b *yamlBuilder) key(n *yaml.Node) (any, error) {
	r := resolveAlias(n)
	if r.Kind == yaml.ScalarNode && r.Style&(yaml.TaggedStyle|yamlQuotedStyles) == 0 &&
		r.Value == "=" {
		return "=", nil
	}
	if r.Kind != yaml.ScalarNode {
		return nil, needsAnsible("a mapping key is not a scalar")
	}
	return b.value(n, false)
}

// scalar constructs a scalar: an explicitly tagged one by its tag, a quoted or block one as a
// string, and a plain one by PyYAML's YAML 1.1 implicit resolution.
func (b *yamlBuilder) scalar(n *yaml.Node, unsafe bool) (any, error) {
	if n.Style&yaml.TaggedStyle != 0 {
		switch tag := n.Tag; tag {
		case "!unsafe":
			v, err := constructScalar(resolvePlain(n.Value), n.Value)
			if err != nil {
				return nil, err
			}
			s, ok := v.(string)
			if !ok {
				return nil, needsAnsible("an !unsafe value is not a string, which Ansible " +
					"releases read differently")
			}
			return unsafeString(s), nil
		case "!vault", "!vault-encrypted":
			return nil, needsAnsible("the YAML holds a vault-encrypted value")
		default:
			short := n.ShortTag()
			switch short {
			case "!!str", "!!int", "!!float", "!!bool", "!!null", "!!timestamp":
				v, err := constructScalar(short, n.Value)
				if err != nil {
					return nil, err
				}
				return markUnsafe(v, unsafe), nil
			}
			return nil, needsAnsible(fmt.Sprintf("the YAML uses the %s tag", tag))
		}
	}
	if n.Style&yamlQuotedStyles != 0 {
		return markUnsafe(n.Value, unsafe), nil
	}
	v, err := constructScalar(resolvePlain(n.Value), n.Value)
	if err != nil {
		return nil, err
	}
	return markUnsafe(v, unsafe), nil
}

// markUnsafe returns a string under !unsafe as an unsafe string, and anything else unchanged.
func markUnsafe(v any, unsafe bool) any {
	if s, ok := v.(string); ok && unsafe {
		return unsafeString(s)
	}
	return v
}

// The YAML 1.1 implicit resolvers PyYAML registers, in its order.
var (
	// yamlBool matches the YAML 1.1 booleans.
	yamlBool = regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|` +
		`on|On|ON|off|Off|OFF)$`)
	// yamlFloat matches the YAML 1.1 floats, which need a decimal point and a signed exponent.
	yamlFloat = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?` +
		`|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?` +
		`|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*` +
		`|[-+]?\.(?:inf|Inf|INF)` +
		`|\.(?:nan|NaN|NAN))$`)
	// yamlInt matches the YAML 1.1 integers: binary, octal by a leading zero, decimal, hex, and
	// base 60.
	yamlInt = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+` +
		`|[-+]?0[0-7_]+` +
		`|[-+]?(?:0|[1-9][0-9_]*)` +
		`|[-+]?0x[0-9a-fA-F_]+` +
		`|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	// yamlNull matches the YAML 1.1 nulls.
	yamlNull = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	// yamlTimestamp matches the YAML 1.1 timestamps PyYAML resolves.
	yamlTimestamp = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]` +
		`|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?` +
		`(?:[Tt]|[ \t]+)[0-9][0-9]?` +
		`:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?` +
		`(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
	// yamlTimestampParts captures a timestamp's fields, as PyYAML's constructor reads them.
	yamlTimestampParts = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)` +
		`(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?` +
		`(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)
)

// resolvePlain returns the tag PyYAML gives a plain scalar, trying the resolvers registered for its
// first character in registration order.
func resolvePlain(v string) string {
	if v == "" {
		return "!!null"
	}
	first := v[0]
	try := func(chars string, re *regexp.Regexp) bool {
		return strings.IndexByte(chars, first) >= 0 && re.MatchString(v)
	}
	switch {
	case try("yYnNtTfFoO", yamlBool):
		return "!!bool"
	case try("-+0123456789.", yamlFloat):
		return "!!float"
	case try("-+0123456789", yamlInt):
		return "!!int"
	case first == '<' && v == "<<":
		return "!!merge"
	case try("~nN", yamlNull):
		return "!!null"
	case try("0123456789", yamlTimestamp):
		return "!!timestamp"
	case first == '=' && v == "=":
		return "!!value"
	}
	return "!!str"
}

// constructScalar builds the value PyYAML's SafeConstructor gives a scalar of the given tag.
func constructScalar(tag, v string) (any, error) {
	switch tag {
	case "!!str":
		return v, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		switch strings.ToLower(v) {
		case "yes", "true", "on":
			return true, nil
		case "no", "false", "off":
			return false, nil
		}
		return nil, invalidf("%q is not a YAML boolean", v)
	case "!!int":
		return constructYAMLInt(v)
	case "!!float":
		return constructYAMLFloat(v)
	case "!!timestamp":
		return constructYAMLTimestamp(v)
	}
	return nil, invalidf("the YAML value %q has no constructor", v)
}

// constructYAMLInt is PyYAML's construct_yaml_int.
func constructYAMLInt(v string) (any, error) {
	v = strings.ReplaceAll(v, "_", "")
	if v == "" {
		return nil, invalidf("an empty YAML integer")
	}
	sign := 1
	if v[0] == '-' {
		sign = -1
	}
	if v[0] == '+' || v[0] == '-' {
		v = v[1:]
	}
	var (
		n  pyNumber
		ok bool
	)
	switch {
	case v == "0":
		return pyNumber{text: "0"}, nil
	case strings.HasPrefix(v, "0b"):
		n, ok = pyInt(sign, v[2:], 2)
	case strings.HasPrefix(v, "0x"):
		n, ok = pyInt(sign, v[2:], 16)
	case v != "" && v[0] == '0':
		n, ok = pyInt(sign, v, 8)
	case strings.Contains(v, ":"):
		return sexagesimalInt(sign, v)
	default:
		n, ok = pyInt(sign, v, 10)
	}
	if !ok || len(n.text) > maxIntDigits {
		return nil, invalidf("%q is not an integer Python reads", v)
	}
	return n, nil
}

// sexagesimalInt reads a base 60 integer such as 1:20 as PyYAML does.
func sexagesimalInt(sign int, v string) (any, error) {
	total := int64(0)
	for _, part := range strings.Split(v, ":") {
		d, err := strconv.ParseInt(part, 10, 64)
		if err != nil || total > (1<<62)/60 {
			return nil, needsAnsible("a base 60 integer is too large")
		}
		total = total*60 + d
	}
	return pyNumber{text: strconv.FormatInt(int64(sign)*total, 10)}, nil
}

// constructYAMLFloat is PyYAML's construct_yaml_float. Infinity and not-a-number are refused, since
// the JSON a composed inventory is rendered as cannot carry them.
func constructYAMLFloat(v string) (any, error) {
	v = strings.ToLower(strings.ReplaceAll(v, "_", ""))
	if v == "" {
		return nil, invalidf("an empty YAML float")
	}
	sign := 1.0
	if v[0] == '-' {
		sign = -1
	}
	if v[0] == '+' || v[0] == '-' {
		v = v[1:]
	}
	if v == ".inf" || v == ".nan" {
		return nil, needsAnsible("a variable is infinite or not a number")
	}
	var f float64
	if strings.Contains(v, ":") {
		base := 1.0
		parts := strings.Split(v, ":")
		for i := len(parts) - 1; i >= 0; i-- {
			d, ok := pyFloatValue(parts[i])
			if !ok {
				return nil, invalidf("%q is not a float Python reads", v)
			}
			f += d * base
			base *= 60
		}
	} else {
		var ok bool
		if f, ok = pyFloatValue(v); !ok {
			return nil, invalidf("%q is not a float Python reads", v)
		}
	}
	n, ok := pyFloat(sign * f)
	if !ok {
		return nil, needsAnsible("a variable is infinite or not a number")
	}
	return n, nil
}

// constructYAMLTimestamp is PyYAML's construct_yaml_timestamp, returning the text Python's
// isoformat writes for the date or datetime, which is what ansible-inventory prints for one.
func constructYAMLTimestamp(v string) (any, error) {
	m := yamlTimestampParts.FindStringSubmatch(v)
	if m == nil {
		return nil, invalidf("%q is not a YAML timestamp", v)
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if year < 1 || date.Year() != year || int(date.Month()) != month || date.Day() != day {
		return nil, invalidf("%q is not a calendar date", v)
	}
	ymd := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
	if m[4] == "" {
		return ymd, nil
	}
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	second, _ := strconv.Atoi(m[6])
	if hour > 23 || minute > 59 || second > 59 {
		return nil, invalidf("%q is not a time of day", v)
	}
	out := ymd + fmt.Sprintf("T%02d:%02d:%02d", hour, minute, second)
	if frac := m[7]; frac != "" {
		if len(frac) > 6 {
			frac = frac[:6]
		}
		frac += strings.Repeat("0", 6-len(frac))
		if frac != "000000" {
			out += "." + frac
		}
	}
	switch {
	case m[9] != "":
		tzHour, _ := strconv.Atoi(m[10])
		tzMinute := 0
		if m[11] != "" {
			tzMinute, _ = strconv.Atoi(m[11])
		}
		offset := tzHour*60 + tzMinute
		if offset >= 24*60 {
			return nil, invalidf("a timestamp's zone offset is out of range")
		}
		// Python prints a zero offset as +00:00 whichever sign it was written with.
		sign := "+"
		if m[9] == "-" && offset != 0 {
			sign = "-"
		}
		out += fmt.Sprintf("%s%02d:%02d", sign, offset/60, offset%60)
	case m[8] == "Z":
		out += "+00:00"
	}
	return out, nil
}

// parseYAMLInventory applies Ansible's YAML inventory plugin to a loaded document. It returns
// errNotYAMLInventory when the plugin declines the document: one that is empty or not a mapping. A
// plugin configuration needs Ansible. A document the plugin would start on and then fail partway
// through is refused, because Ansible would go on to read the same text as INI on top of the half
// it had read.
func parseYAMLInventory(d *invData, doc any) error {
	if !pyTruthy(doc) {
		return errNotYAMLInventory
	}
	m, ok := doc.(*orderedMap)
	if !ok {
		return errNotYAMLInventory
	}
	if p, ok := m.Get("plugin"); ok && pyTruthy(p) {
		return needsAnsible("it is an inventory plugin configuration, which only Ansible runs")
	}
	for _, e := range m.entries {
		name, ok := e.key.(string)
		if !ok {
			if _, isMap := e.value.(*orderedMap); isMap || e.value == nil {
				return invalidf("a top-level group name is not a string")
			}
			// Ansible skips a group whose definition is not a mapping before it looks at the name.
			continue
		}
		if _, err := yamlGroup(d, name, e.value); err != nil {
			return err
		}
	}
	return nil
}

// yamlGroup is the YAML plugin's _parse_group. A group whose definition is neither a mapping nor
// empty is skipped, as Ansible skips it with a warning. It returns the group's name.
func yamlGroup(d *invData, name string, data any) (bool, error) {
	if data != nil {
		if _, ok := data.(*orderedMap); !ok {
			return false, nil
		}
	}
	if name == "" {
		return false, invalidf("a group name is empty")
	}
	d.addGroup(name)
	if data == nil {
		return true, nil
	}
	m := data.(*orderedMap)
	sections := map[string]any{}
	for _, section := range []string{"vars", "children", "hosts"} {
		v, ok := m.Get(section)
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			one := newOrderedMap()
			one.Set(t, nil)
			v = one
		case unsafeString:
			one := newOrderedMap()
			one.Set(string(t), nil)
			v = one
		}
		if _, isMap := v.(*orderedMap); v != nil && !isMap {
			return false, invalidf("invalid %q entry for %q group, requires a dictionary",
				section, name)
		}
		sections[section] = v
	}
	for _, e := range m.entries {
		key, _ := e.key.(string)
		value := e.value
		if v, ok := sections[key]; ok {
			value = v
		}
		sub, ok := value.(*orderedMap)
		if !ok {
			// Not a mapping: skipped with a warning, or empty and skipped quietly.
			continue
		}
		switch key {
		case "vars":
			for _, ve := range sub.entries {
				vname, ok := ve.key.(string)
				if !ok {
					return false, needsAnsible("a variable name is not a string")
				}
				if err := d.setVariable(name, vname, ve.value); err != nil {
					return false, err
				}
			}
		case "children":
			for _, ce := range sub.entries {
				child, ok := ce.key.(string)
				if !ok {
					return false, invalidf("a child group name is not a string")
				}
				added, err := yamlGroup(d, child, ce.value)
				if err != nil {
					return false, err
				}
				if !added {
					return false, needsAnsible(fmt.Sprintf("child group %q of %q is not a "+
						"mapping, which Ansible reads by whatever already has that name", child, name))
				}
				if err := d.addChild(name, child); err != nil {
					return false, invalidf("%s", err.Error())
				}
			}
		case "hosts":
			for _, he := range sub.entries {
				pattern, ok := he.key.(string)
				if !ok {
					return false, invalidf("a host name is not a string; quote a host named " +
						"by a number")
				}
				if err := yamlHost(d, name, pattern, he.value); err != nil {
					return false, err
				}
			}
		}
	}
	return true, nil
}

// yamlHost adds the hosts one entry of a group's hosts names, with their variables, as the YAML
// plugin's _parse_host and _populate_host_vars do.
func yamlHost(d *invData, group, pattern string, vars any) error {
	hosts, port, err := expandHostPattern(pattern, d.limit-len(d.hosts))
	if err != nil {
		var needs *NeedsAnsibleError
		if errors.As(err, &needs) || errors.Is(err, ErrTooManyHosts) {
			return err
		}
		return invalidf("%s", err.Error())
	}
	var vm *orderedMap
	switch t := vars.(type) {
	case *orderedMap:
		vm = t
	default:
		if pyTruthy(vars) {
			return invalidf("host %q has variables that are not a mapping", pattern)
		}
		vm = newOrderedMap()
	}
	for _, h := range hosts {
		if err := d.addHost(h, group, port); err != nil {
			return err
		}
		for _, e := range vm.entries {
			k, ok := e.key.(string)
			if !ok {
				return needsAnsible("a variable name is not a string")
			}
			if err := d.setVariable(h, k, e.value); err != nil {
				return err
			}
		}
	}
	return nil
}
