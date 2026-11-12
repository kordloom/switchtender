package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The constructed plugin evaluates Jinja expressions an admin wrote, and those expressions read the
// names and variables of every input. Ansible templates a string it reads from an inventory, so an
// input's data is code the moment an expression reaches it, and the evaluation happens on this
// server before any policy, hold, or approval. An input is therefore read here first, refused when
// any name, key, or value in it could be template text, and handed to Ansible as a rendering of the
// model read here, with every string marked as not to be templated, never as the bytes it is stored
// as.

// templateMarkers are the strings that start template code in a value Ansible templates: the
// three Jinja delimiters, and the header that changes them for the rest of the string.
var templateMarkers = []string{"{{", "{%", "{#", "#jinja2:"}

// decoderKeys are the mapping keys Ansible's JSON reader takes as a tagged value rather than a
// mapping: a string not to be templated, and a vault-encrypted value.
var decoderKeys = []string{"__ansible_unsafe", "__ansible_vault"}

// unsafeKey is the key of the JSON object Ansible reads as a string it will not template.
const unsafeKey = "__ansible_unsafe"

// HasTemplateMarker reports whether s holds any of the strings that start template code.
func HasTemplateMarker(s string) bool {
	for _, m := range templateMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ConstructedInput reads one input of a constructed inventory and returns the document Ansible is
// given in its place.
//
// The input is read with the native engine, and anything it cannot read is refused, so Ansible
// never reads what this did not. Every host, group, and child name, every variable name and nested
// key, and every string value at any depth is checked, including the group all, groups with no
// host, reserved names, and values already marked as not to be templated. A refusal names where in
// the input the problem is and the rule it breaks, never the value. What passes is written as JSON
// that keeps each group's hosts, children, and variables where the input put them, with every
// string as an object Ansible reads as a string it will not template.
func ConstructedInput(content string) ([]byte, error) {
	if strings.TrimSpace(content) == "{}" {
		// Ansible reads a lone {} as one host named {}, and an inventory source that has not synced
		// yet holds exactly this, so the refusal says so rather than naming a brace.
		return nil, refuseInput("it holds only {}, which Ansible reads as one host named {}. An " +
			"inventory source that has not synced yet holds this, so sync it first")
	}
	d, err := nativeData(content)
	if err != nil {
		return nil, readerRefusal(err)
	}
	if err := d.scanTemplateText(); err != nil {
		return nil, err
	}
	if _, err := d.listing(); err != nil {
		return nil, readerRefusal(err)
	}
	return d.render()
}

// readerRefusal explains a native engine error on a constructed inventory's input as text, so the
// error the input is refused with never matches ErrNeedsAnsible: installing Ansible does not change
// the answer.
func readerRefusal(err error) error {
	reason := err.Error()
	if r, ok := NeedsAnsibleReason(err); ok {
		reason = r
	}
	return fmt.Errorf("%w: every input of a constructed inventory is read by SwitchTender before "+
		"Ansible sees it, and this one cannot be: %s", ErrConstructedInput, reason)
}

// scanTemplateText refuses the model when any name, key, or string in it breaks the rules for a
// constructed inventory's input.
func (d *invData) scanTemplateText() error {
	for _, name := range d.groupOrder {
		g := d.groups[name]
		// A child group is a group of its own in the model, so its name is checked here too.
		if rule := nameRule(name); rule != "" {
			return refuseInput("a group name %s", rule)
		}
		if err := scanVarMap(g.vars, fmt.Sprintf("group %q", name)); err != nil {
			return err
		}
	}
	for _, name := range d.hostOrder {
		if rule := nameRule(name); rule != "" {
			return refuseInput("a host name %s", rule)
		}
		if err := scanVarMap(d.hosts[name].vars, fmt.Sprintf("host %q", name)); err != nil {
			return err
		}
	}
	return nil
}

// scanVarMap checks the variables of one host or group, named by owner.
func scanVarMap(m *varMap, owner string) error {
	for _, key := range m.keys {
		if rule := nameRule(key); rule != "" {
			return refuseInput("a variable name of %s %s", owner, rule)
		}
		if err := scanValue(m.values[key], owner, key); err != nil {
			return err
		}
	}
	return nil
}

// scanValue checks one value at path in the variables of owner, and everything nested in it.
func scanValue(v any, owner, path string) error {
	switch t := v.(type) {
	case nil, bool, pyNumber:
		return nil
	case string:
		return scanString(t, owner, path)
	case unsafeString:
		// A value written as not to be templated gets no exemption: the mark is the input's own
		// claim, and an input is what is being checked.
		return scanString(string(t), owner, path)
	case []any:
		for i, item := range t {
			if err := scanValue(item, owner, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	case *orderedMap:
		for _, e := range t.entries {
			var key string
			switch k := e.key.(type) {
			case string:
				if rule := nameRule(k); rule != "" {
					return refuseInput("a key under %s in the variables of %s %s", path, owner, rule)
				}
				key = k
			case pyNumber:
				key = k.text
			default:
				return refuseInput("a key under %s in the variables of %s is of a kind this does "+
					"not read", path, owner)
			}
			if err := scanValue(e.value, owner, path+"."+key); err != nil {
				return err
			}
		}
		return nil
	}
	return refuseInput("variable %s of %s is of a kind this does not read", path, owner)
}

// scanString refuses a string value at path that holds template text.
func scanString(s, owner, path string) error {
	if HasTemplateMarker(s) {
		return refuseInput("variable %s of %s holds template text, which Ansible would evaluate "+
			"on this server while it builds the inventory, before anyone approves the run", path, owner)
	}
	return nil
}

// nameRule returns the rule a host, group, variable, or key name breaks, in words that finish a
// sentence about it, or "" when it breaks none. A name cannot be marked as not to be templated the
// way a value can, so besides template text a name may hold no brace and no control character, and
// it may not be a key Ansible's JSON reader takes as a tagged value.
func nameRule(name string) string {
	switch {
	case HasTemplateMarker(name):
		return "holds template text"
	case strings.ContainsAny(name, "{}"):
		return "holds a brace, which a name of a constructed inventory's input may not"
	case strings.ContainsFunc(name, unicode.IsControl):
		return "holds a control character, which a name of a constructed inventory's input may not"
	case slices.Contains(decoderKeys, name):
		return "is a key Ansible reads as a tagged value rather than a name"
	}
	return ""
}

// refuseInput returns ErrConstructedInput with the formatted reason.
func refuseInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConstructedInput, fmt.Sprintf(format, args...))
}

// jsonField is one member of a jsonObject.
type jsonField struct {
	// Key is the member's name.
	Key string
	// Value is the member's value.
	Value any
}

// jsonObject is a JSON object written with its members in order. Ansible's inventory plugins keep a
// document's order, which decides host order and the order groups are made in.
type jsonObject []jsonField

// MarshalJSON writes the members in order.
func (o jsonObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(f.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// render writes the model as an inventory document in JSON that Ansible's YAML plugin reads: one
// member per group with its direct hosts, its child groups, and its own variables, each host's own
// variables given where it first appears. Every string value is written as an object Ansible reads
// as a string it will not template.
func (d *invData) render() ([]byte, error) {
	written := map[string]bool{}
	doc := make(jsonObject, 0, len(d.groupOrder))
	for _, name := range d.groupOrder {
		g := d.groups[name]
		var group jsonObject
		if len(g.hosts) > 0 {
			hosts := make(jsonObject, 0, len(g.hosts))
			for _, h := range g.hosts {
				var vars any
				if !written[h.name] && len(h.vars.keys) > 0 {
					v, err := renderVarMap(h.vars)
					if err != nil {
						return nil, err
					}
					vars = v
				}
				written[h.name] = true
				hosts = append(hosts, jsonField{Key: h.name, Value: vars})
			}
			group = append(group, jsonField{Key: "hosts", Value: hosts})
		}
		if len(g.children) > 0 {
			children := make(jsonObject, 0, len(g.children))
			for _, c := range g.children {
				children = append(children, jsonField{Key: c.name, Value: nil})
			}
			group = append(group, jsonField{Key: "children", Value: children})
		}
		vars, err := renderVarMap(g.vars)
		if err != nil {
			return nil, err
		}
		// The model keeps ansible_group_priority as the group's priority and not as a variable,
		// so it is written back as the variable that sets it.
		if g.priority != 1 {
			vars = append(vars, jsonField{Key: "ansible_group_priority",
				Value: json.Number(strconv.FormatInt(g.priority, 10))})
		}
		if len(vars) > 0 {
			group = append(group, jsonField{Key: "vars", Value: vars})
		}
		if group == nil {
			group = jsonObject{}
		}
		doc = append(doc, jsonField{Key: name, Value: group})
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: write the input for Ansible: %w", ErrConstructedInput, err)
	}
	return out, nil
}

// renderVarMap writes a host's or group's variables in order.
func renderVarMap(m *varMap) (jsonObject, error) {
	out := make(jsonObject, 0, len(m.keys))
	for _, k := range m.keys {
		v, err := renderValue(m.values[k])
		if err != nil {
			return nil, err
		}
		out = append(out, jsonField{Key: k, Value: v})
	}
	return out, nil
}

// renderValue writes one value with every string marked as not to be templated. A mapping key that
// is a number is written as its digits, which is how ansible-inventory --list prints it.
func renderValue(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool:
		return t, nil
	case string:
		return jsonObject{{Key: unsafeKey, Value: t}}, nil
	case unsafeString:
		return jsonObject{{Key: unsafeKey, Value: string(t)}}, nil
	case pyNumber:
		return json.Number(t.text), nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			c, err := renderValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *orderedMap:
		out := make(jsonObject, 0, t.Len())
		for _, e := range t.entries {
			var key string
			switch k := e.key.(type) {
			case string:
				key = k
			case pyNumber:
				key = k.text
			}
			c, err := renderValue(e.value)
			if err != nil {
				return nil, err
			}
			out = append(out, jsonField{Key: key, Value: c})
		}
		return out, nil
	}
	return nil, refuseInput("a value is of a kind this does not read")
}

// UnwrapUnsafe turns every object in the listing's host variables that holds only a string marked
// as not to be templated, with no template text in it, back into the plain string, at every depth.
// Ansible prints each string of a rendered input in that object form, and a play reading an
// inventory variable expects the string itself. A string that holds template text keeps the object
// form, so a value a constructed expression composed into template text reaches the play as text
// Ansible will not run.
func (l *Listing) UnwrapUnsafe() {
	for host, vars := range l.hostVars {
		for k, v := range vars {
			vars[k] = unwrapUnsafe(v)
		}
		l.hostVars[host] = vars
	}
}

// unwrapUnsafe returns v with every marker-free unsafe object replaced by its string.
func unwrapUnsafe(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if s, ok := t[unsafeKey].(string); ok {
				if HasTemplateMarker(s) {
					return t
				}
				return s
			}
		}
		for k, item := range t {
			t[k] = unwrapUnsafe(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = unwrapUnsafe(item)
		}
		return t
	}
	return v
}
