package inventory

import (
	"strings"
	"unicode/utf8"
)

// Setting is one variable an inventory document sets, on a group or on a host, as it is written
// there rather than as it folds into a host's resolved variables. A group's own variables matter
// even when the group holds no host, since Ansible's implicit localhost takes the variables of all.
type Setting struct {
	// Group is the group the variable is set on, empty for a host's own variable.
	Group string
	// Host is the host the variable is set on, empty for a group's variable.
	Host string
	// Name is the variable's name.
	Name string
	// Value is the variable's value: a string, bool, nil, number text, slice, or string-keyed map.
	// A string Ansible was told never to template is an Unsafe value instead of a string.
	Value any
}

// Unsafe is a string an inventory marks with !unsafe or __ansible_unsafe, which Ansible hands on
// without templating it.
type Unsafe string

// Reading is what one way of reading an inventory document found: every host it names, and every
// variable it sets on a group or a host, in the order the document adds them.
type Reading struct {
	// Hosts are the host names the document names.
	Hosts []string
	// Settings are the variables the document sets.
	Settings []Setting
}

// ReadAsAnsible reads content the way Ansible reads a file with no extension, or a YAML or JSON
// one: the YAML plugin first, and the INI plugin when the YAML plugin declines the file. It fails
// the way ResolveNative does.
func ReadAsAnsible(content string) (*Reading, error) {
	d, err := nativeData(content)
	if err != nil {
		return nil, err
	}
	return d.reading()
}

// ReadAsINI reads content the way Ansible's INI plugin alone does, which is how Ansible reads a
// file whose extension the YAML plugin does not accept, such as hosts.ini.
func ReadAsINI(content string) (*Reading, error) {
	if err := readable(content); err != nil {
		return nil, err
	}
	d := newInvData()
	if err := parseINI(d, content); err != nil {
		return nil, err
	}
	if err := d.reconcile(); err != nil {
		return nil, invalidf("%s", err.Error())
	}
	return d.reading()
}

// YAMLExtension reports whether Ansible's YAML plugin accepts a file named name: one with no
// extension, or a .yml, .yaml, or .json one. Every other file goes to the INI plugin alone.
func YAMLExtension(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	i := strings.LastIndex(base, ".")
	if i <= 0 {
		return true
	}
	switch strings.ToLower(base[i:]) {
	case ".yml", ".yaml", ".json":
		return true
	}
	return false
}

// readable refuses content no Ansible inventory plugin reads as plain text: content that is not
// UTF-8, starts with a byte order mark, or is a whole file encrypted with Ansible Vault. Ansible
// decrypts a file that starts with the vault header before any plugin reads it, and the header is
// matched at the first byte only, so the check does not trim either.
func readable(content string) error {
	switch {
	case !utf8.ValidString(content):
		return needsAnsible("its content is not valid UTF-8")
	case strings.HasPrefix(content, "\ufeff"):
		return needsAnsible("its content starts with a byte order mark")
	case strings.HasPrefix(content, "$ANSIBLE_VAULT"):
		return needsAnsible("it is a whole file encrypted with Ansible Vault")
	}
	return nil
}

// reading lists every host and every variable set on a group or a host in d.
func (d *invData) reading() (*Reading, error) {
	out := &Reading{Hosts: append([]string(nil), d.hostOrder...)}
	for _, name := range d.groupOrder {
		g := d.groups[name]
		for _, k := range g.vars.keys {
			v, err := settingValue(g.vars.values[k])
			if err != nil {
				return nil, err
			}
			out.Settings = append(out.Settings, Setting{Group: name, Name: k, Value: v})
		}
	}
	for _, name := range d.hostOrder {
		h := d.hosts[name]
		for _, k := range h.vars.keys {
			v, err := settingValue(h.vars.values[k])
			if err != nil {
				return nil, err
			}
			out.Settings = append(out.Settings, Setting{Host: name, Name: k, Value: v})
		}
	}
	return out, nil
}

// settingValue converts a value the engine built into plain Go values, keeping a string Ansible
// will not template apart from one it will. A mapping key is kept as written, a number key
// included, since a key is templated like a value.
func settingValue(v any) (any, error) {
	switch t := v.(type) {
	case unsafeString:
		return Unsafe(t), nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			c, err := settingValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *orderedMap:
		out := make(map[string]any, t.Len())
		for _, e := range t.entries {
			var key string
			switch k := e.key.(type) {
			case string:
				key = k
			case pyNumber:
				key = k.text
			}
			c, err := settingValue(e.value)
			if err != nil {
				return nil, err
			}
			out[key] = c
		}
		return out, nil
	}
	return listingValue(v)
}
