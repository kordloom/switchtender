package inventory

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This file is Ansible's INI inventory plugin, line for line: the same sections, the same handling
// of groups named only as children until declared later, host lines split with shlex, and every
// value, on a host line or under :vars, typed with Python's literal_eval.

// iniPending is a group named before it is declared: as the child of a [parent:children] section,
// or by a [group:vars] section alone.
type iniPending struct {
	// line is the line that named it, for the error.
	line int
	// state is children or vars, the section that named it.
	state string
	// parents are the groups that named it as a child, in order.
	parents []string
}

// parseINI reads content as Ansible's INI plugin does into d.
func parseINI(d *invData, content string) error {
	pending := map[string]*iniPending{}
	var pendingOrder []string
	groupname, state := "ungrouped", "hosts"
	for n, raw := range pySplitLines(content) {
		lineNum := n + 1
		line := pyStrip(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if name, tag, ok := iniSection(line); ok {
			groupname = name
			state = tag
			if state == "" {
				state = "hosts"
			}
			if state != "hosts" && state != "children" && state != "vars" {
				return iniError(lineNum, fmt.Sprintf("section [%s:%s] has unknown type: %s",
					name, tag, state))
			}
			if _, ok := d.groups[groupname]; !ok {
				if _, isPending := pending[groupname]; state == "vars" && !isPending {
					pending[groupname] = &iniPending{line: lineNum, state: state}
					pendingOrder = append(pendingOrder, groupname)
				}
				d.addGroup(groupname)
			}
			if p, ok := pending[groupname]; ok && state != "vars" {
				if p.state == "children" {
					if err := addPendingChildren(d, groupname, pending); err != nil {
						return iniError(lineNum, err.Error())
					}
				} else {
					delete(pending, groupname)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			return iniError(lineNum, fmt.Sprintf("invalid section entry: '%s'. Please make sure "+
				"that there are no spaces in the section entry, and that there are no other "+
				"invalid characters", line))
		}
		var err error
		switch state {
		case "hosts":
			err = iniHostLine(d, line, groupname)
		case "vars":
			err = iniVarLine(d, line, groupname)
		case "children":
			child, ok := iniGroupName(line)
			if !ok {
				return iniError(lineNum, "expected group name, got: "+line)
			}
			if _, known := d.groups[child]; !known {
				if p, isPending := pending[child]; isPending {
					p.parents = append(p.parents, groupname)
				} else {
					pending[child] = &iniPending{line: lineNum, state: "children",
						parents: []string{groupname}}
					pendingOrder = append(pendingOrder, child)
				}
			} else {
				err = d.addChild(groupname, child)
			}
		}
		if err != nil {
			var needs *NeedsAnsibleError
			if errors.As(err, &needs) || errors.Is(err, ErrTooManyHosts) {
				return err
			}
			return iniError(lineNum, err.Error())
		}
	}
	for _, name := range pendingOrder {
		p, ok := pending[name]
		if !ok {
			continue
		}
		if p.state == "vars" {
			return iniError(p.line, fmt.Sprintf("section [%s:vars] not valid for undefined "+
				"group %q", name, name))
		}
		return iniError(p.line, fmt.Sprintf("section [%s:children] includes undefined group %q",
			p.parents[len(p.parents)-1], name))
	}
	return nil
}

// iniError wraps a parse failure with its line, the way Ansible reports one.
func iniError(line int, msg string) error {
	return fmt.Errorf("%w: line %d: %s", ErrInvalidInventory, line, msg)
}

// addPendingChildren adds group to every parent that named it before it was declared, and does the
// same for each of those parents that was itself waiting, as _add_pending_children does.
func addPendingChildren(d *invData, group string, pending map[string]*iniPending) error {
	for _, parent := range pending[group].parents {
		if err := d.addChild(parent, group); err != nil {
			return err
		}
		if p, ok := pending[parent]; ok && p.state == "children" {
			if err := addPendingChildren(d, parent, pending); err != nil {
				return err
			}
		}
	}
	delete(pending, group)
	return nil
}

// iniSection matches a section header: [name] or [name:tag], where the name holds no colon, closing
// bracket, or whitespace and the tag is word characters, optionally followed by whitespace and a #
// comment. It returns the name and the tag, empty when there is none.
func iniSection(line string) (string, string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", "", false
	}
	i := 1
	for i < len(line) {
		r, size := utf8.DecodeRuneInString(line[i:])
		if r == ':' || r == ']' || pyIsSpace(r) {
			break
		}
		i += size
	}
	if i == 1 {
		return "", "", false
	}
	name := line[1:i]
	tag := ""
	if i < len(line) && line[i] == ':' {
		j := i + 1
		for j < len(line) {
			r, size := utf8.DecodeRuneInString(line[j:])
			if !pyIsWord(r) {
				break
			}
			j += size
		}
		if j == i+1 {
			return "", "", false
		}
		tag = line[i+1 : j]
		i = j
	}
	if i >= len(line) || line[i] != ']' {
		return "", "", false
	}
	if !iniTrailer(line[i+1:]) {
		return "", "", false
	}
	return name, tag, true
}

// iniTrailer reports whether rest is whitespace optionally followed by a # comment.
func iniTrailer(rest string) bool {
	rest = strings.TrimLeftFunc(rest, pyIsSpace)
	return rest == "" || rest[0] == '#'
}

// iniGroupName matches a [group:children] line: a name with no colon, closing bracket, or
// whitespace, optionally followed by whitespace and a # comment.
func iniGroupName(line string) (string, bool) {
	i := 0
	for i < len(line) {
		r, size := utf8.DecodeRuneInString(line[i:])
		if r == ':' || r == ']' || pyIsSpace(r) {
			break
		}
		i += size
	}
	if i == 0 || !iniTrailer(line[i:]) {
		return "", false
	}
	return line[:i], true
}

// iniVarLine reads a key=value line of a [group:vars] section. Everything after the first = is the
// value, stripped and typed with literal_eval.
func iniVarLine(d *invData, line, group string) error {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return fmt.Errorf("expected key=value, got: %s", line)
	}
	value, err := pyParseValue(pyStrip(v))
	if err != nil {
		return err
	}
	return d.setVariable(group, pyStrip(k), value)
}

// iniHostLine reads a host line: a host name or pattern, then key=value assignments, split the way
// shlex splits them, each value typed with literal_eval.
func iniHostLine(d *invData, line, group string) error {
	tokens, err := pyShlexSplit(line)
	if err != nil {
		return fmt.Errorf("error parsing host definition '%s': %s", line, err)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("error parsing host definition '%s'", line)
	}
	hosts, port, err := iniExpandHostPattern(tokens[0], d.limit-len(d.hosts))
	if err != nil {
		return err
	}
	vars := newVarMap()
	for _, t := range tokens[1:] {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return fmt.Errorf("expected key=value host variable assignment, got: %s", t)
		}
		value, err := pyParseValue(v)
		if err != nil {
			return err
		}
		vars.set(k, value)
	}
	for _, h := range hosts {
		if err := d.addHost(h, group, port); err != nil {
			return err
		}
		for _, k := range vars.keys {
			if err := d.setVariable(h, k, vars.values[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// iniExpandHostPattern is the INI plugin's _expand_hostpattern: the base expansion, refusing an
// entry that ends in a colon with no port after it and a host named ---, which is a sign the file
// is YAML.
func iniExpandHostPattern(entry string, limit int) ([]string, string, error) {
	hosts, port, err := expandHostPattern(entry, limit)
	if err != nil {
		return nil, "", err
	}
	if strings.HasSuffix(pyStrip(entry), ":") && port == "" {
		return nil, "", fmt.Errorf("invalid host pattern '%s' supplied, ending in ':' is not "+
			"allowed, this character is reserved to provide a port", entry)
	}
	for _, h := range hosts {
		if pyStrip(h) == "---" {
			return nil, "", fmt.Errorf("invalid host pattern '%s' supplied, '---' is normally a "+
				"sign this is a YAML file", entry)
		}
	}
	return hosts, port, nil
}
