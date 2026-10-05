package inventory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file reproduces Ansible's inventory data model, InventoryData with its Group and Host
// objects, closely enough that the same sequence of additions leaves the same groups, the same
// memberships, the same group depths and priorities, and the same variables. The parsers drive it
// in the order Ansible's INI and YAML plugins do, and the listing it produces is the one
// ansible-inventory --list prints.

// errNotKnown is returned when a child named in a group is neither a group nor a host.
var errNotKnown = errors.New("is not a known host nor group")

// invGroup is one Ansible group.
type invGroup struct {
	// name is the group's name.
	name string
	// depth is how far below all the group sits, by the longest path, as Ansible computes it.
	depth int
	// priority is the ansible_group_priority the group was given, 1 by default.
	priority int64
	// vars are the group's own variables.
	vars *varMap
	// hosts are the group's direct members, in the order they were added.
	hosts []*invHost
	// hostNames indexes hosts by name.
	hostNames map[string]bool
	// children are the group's child groups, in the order they were added.
	children []*invGroup
	// parents are the group's parent groups, in the order they were added.
	parents []*invGroup
}

// invHost is one Ansible host.
type invHost struct {
	// name is the inventory host name.
	name string
	// vars are the host's own variables.
	vars *varMap
	// groups are every group the host belongs to, directly or through a child group, in the order
	// Ansible added them.
	groups []*invGroup
}

// varMap is a variable mapping that keeps insertion order, as a Python dict does.
type varMap struct {
	// keys are the variable names in insertion order.
	keys []string
	// values maps a name to its value.
	values map[string]any
}

// newVarMap returns an empty varMap.
func newVarMap() *varMap { return &varMap{values: map[string]any{}} }

// set stores a variable, replacing any value it had. Ansible merges two mappings only when it is
// configured to merge hashes, which is not the default, so a later value replaces an earlier one
// whole.
func (m *varMap) set(key string, value any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// invData is Ansible's InventoryData: every group and host by name, in the order they were added.
type invData struct {
	// groups maps a group name to the group.
	groups map[string]*invGroup
	// groupOrder lists group names in the order they were added.
	groupOrder []string
	// hosts maps a host name to the host.
	hosts map[string]*invHost
	// hostOrder lists host names in the order they were added.
	hostOrder []string
	// limit bounds how many hosts the inventory may hold.
	limit int
}

// newInvData returns an inventory holding the all and ungrouped groups, ungrouped a child of all,
// as Ansible starts every inventory.
func newInvData() *invData {
	d := &invData{groups: map[string]*invGroup{}, hosts: map[string]*invHost{}, limit: MaxHosts}
	d.addGroup("all")
	d.addGroup("ungrouped")
	_ = d.addChild("all", "ungrouped")
	return d
}

// addGroup adds a group unless one of that name exists, and returns its name.
func (d *invData) addGroup(name string) string {
	if _, ok := d.groups[name]; !ok {
		d.groups[name] = &invGroup{name: name, priority: 1, vars: newVarMap(),
			hostNames: map[string]bool{}}
		d.groupOrder = append(d.groupOrder, name)
	}
	return name
}

// addHost adds a host unless one of that name exists, and makes it a member of group when one is
// named. A port is set as ansible_port only when the host is new and the port is not zero, which is
// what Ansible's Host constructor does.
func (d *invData) addHost(name, group string, port string) error {
	if name == "" {
		return fmt.Errorf("invalid empty host name provided")
	}
	var g *invGroup
	if group != "" {
		g = d.groups[group]
		if g == nil {
			return fmt.Errorf("could not find group %s in inventory", group)
		}
	}
	h, ok := d.hosts[name]
	if !ok {
		if len(d.hosts) >= d.limit {
			return ErrTooManyHosts
		}
		h = &invHost{name: name, vars: newVarMap()}
		d.hosts[name] = h
		d.hostOrder = append(d.hostOrder, name)
		if port != "" {
			n, ok := pyInt(1, strings.TrimLeft(port, "0"), 10)
			if strings.TrimLeft(port, "0") == "" {
				n, ok = pyNumber{text: "0"}, true
			}
			if ok && n.text != "0" {
				h.vars.set("ansible_port", n)
			}
		}
	}
	if g != nil {
		g.addHost(h)
	}
	return nil
}

// addHost makes h a direct member of g and of every ancestor of g, through h's groups.
func (g *invGroup) addHost(h *invHost) {
	if g.hostNames[h.name] {
		return
	}
	g.hosts = append(g.hosts, h)
	g.hostNames[h.name] = true
	h.addGroup(g)
}

// addGroup records g and its ancestors among the host's groups, ancestors first.
func (h *invHost) addGroup(g *invGroup) {
	for _, a := range g.ancestors() {
		if !h.inGroup(a) {
			h.groups = append(h.groups, a)
		}
	}
	if !h.inGroup(g) {
		h.groups = append(h.groups, g)
	}
}

// inGroup reports whether g is among the host's groups.
func (h *invHost) inGroup(g *invGroup) bool {
	for _, have := range h.groups {
		if have == g {
			return true
		}
	}
	return false
}

// ancestors returns every group above g.
func (g *invGroup) ancestors() []*invGroup {
	var out []*invGroup
	seen := map[*invGroup]bool{}
	queue := append([]*invGroup(nil), g.parents...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		queue = append(queue, p.parents...)
	}
	return out
}

// descendantHosts returns every host in g or a group below it.
func (g *invGroup) descendantHosts() []*invHost {
	var out []*invHost
	seenHost := map[*invHost]bool{}
	seenGroup := map[*invGroup]bool{}
	queue := []*invGroup{g}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seenGroup[c] {
			continue
		}
		seenGroup[c] = true
		for _, h := range c.hosts {
			if !seenHost[h] {
				seenHost[h] = true
				out = append(out, h)
			}
		}
		queue = append(queue, c.children...)
	}
	return out
}

// addChild adds child, a group or a host, to group, as InventoryData.add_child does.
func (d *invData) addChild(group, child string) error {
	g := d.groups[group]
	if g == nil {
		return fmt.Errorf("%s is not a known group", group)
	}
	if c := d.groups[child]; c != nil {
		return g.addChildGroup(c)
	}
	if h := d.hosts[child]; h != nil {
		g.addHost(h)
		return nil
	}
	return fmt.Errorf("%s %w", child, errNotKnown)
}

// addChildGroup makes c a child of g: it refuses a loop, raises the depth of c and everything below
// it, and adds g and its ancestors to the groups of every host under c, as Group.add_child_group
// does.
func (g *invGroup) addChildGroup(c *invGroup) error {
	if g == c {
		return fmt.Errorf("can't add group to itself")
	}
	for _, have := range g.children {
		if have == c {
			return nil
		}
	}
	startAncestors := c.ancestors()
	newAncestors := g.ancestors()
	for _, a := range newAncestors {
		if a == c {
			return fmt.Errorf("adding group '%s' as child to '%s' creates a recursive dependency "+
				"loop", c.name, g.name)
		}
	}
	newAncestors = append(newAncestors, g)
	g.children = append(g.children, c)
	if g.depth+1 > c.depth {
		c.depth = g.depth + 1
	}
	if err := c.checkChildrenDepth(); err != nil {
		return err
	}
	isParent := false
	for _, p := range c.parents {
		if p.name == g.name {
			isParent = true
		}
	}
	if !isParent {
		c.parents = append(c.parents, g)
		added := make([]*invGroup, 0, len(newAncestors))
		for _, a := range newAncestors {
			if !containsGroup(startAncestors, a) {
				added = append(added, a)
			}
		}
		for _, h := range c.descendantHosts() {
			for _, a := range added {
				if !h.inGroup(a) {
					h.groups = append(h.groups, a)
				}
			}
		}
	}
	return nil
}

// containsGroup reports whether g is in list.
func containsGroup(list []*invGroup, g *invGroup) bool {
	for _, have := range list {
		if have == g {
			return true
		}
	}
	return false
}

// checkChildrenDepth raises the depth of every group below g to at least one more than its parent,
// level by level, as Group._check_children_depth does, and refuses a loop.
func (g *invGroup) checkChildrenDepth() error {
	depth := g.depth
	start := g.depth
	seen := map[*invGroup]bool{}
	unprocessed := map[*invGroup]bool{}
	for _, c := range g.children {
		unprocessed[c] = true
	}
	for len(unprocessed) > 0 {
		for c := range unprocessed {
			seen[c] = true
		}
		depth++
		next := map[*invGroup]bool{}
		for c := range unprocessed {
			if c.depth < depth {
				c.depth = depth
				for _, cc := range c.children {
					next[cc] = true
				}
			}
		}
		unprocessed = next
		if depth-start > len(seen) {
			return fmt.Errorf("the group named '%s' has a recursive dependency loop", g.name)
		}
	}
	return nil
}

// setVariable sets a variable on the group or the host named entity, as InventoryData.set_variable
// does: a group of that name wins over a host of that name, so a host sharing a group's name has
// its variables set on the group. ansible_group_priority is not a variable on a group: it sets the
// group's priority and is not stored.
func (d *invData) setVariable(entity, key string, value any) error {
	if g := d.groups[entity]; g != nil {
		if key == "ansible_group_priority" {
			p, err := groupPriority(value)
			if err != nil {
				return err
			}
			g.priority = p
			return nil
		}
		g.vars.set(key, value)
		return nil
	}
	if h := d.hosts[entity]; h != nil {
		h.vars.set(key, value)
		return nil
	}
	return fmt.Errorf("could not identify group or host named %s", entity)
}

// groupPriority reads an ansible_group_priority value as Python's int() does for the forms
// Ansible releases agree on: an int, or a string of an int. Anything else is left to Ansible,
// whose releases disagree about it.
func groupPriority(v any) (int64, error) {
	switch t := v.(type) {
	case pyNumber:
		if !t.float {
			if n, ok := pyIntText(t.text); ok {
				return n, nil
			}
		}
	case string:
		if n, ok := pyIntText(t); ok {
			return n, nil
		}
	}
	return 0, needsAnsible("an ansible_group_priority is not a whole number")
}

// reconcile applies Ansible's reconcile_inventory: every group with no parent becomes a child of
// all, a host filed under ungrouped that belongs to another group leaves ungrouped, and a host that
// belongs to nothing but all joins ungrouped.
func (d *invData) reconcile() error {
	for _, name := range d.groupOrder {
		g := d.groups[name]
		if name != "all" && len(g.ancestors()) == 0 {
			if err := d.addChild("all", name); err != nil {
				return err
			}
		}
	}
	all, ungrouped := d.groups["all"], d.groups["ungrouped"]
	for _, name := range d.hostOrder {
		h := d.hosts[name]
		if h.inGroup(ungrouped) {
			others := false
			for _, g := range h.groups {
				if g != all && g != ungrouped {
					others = true
				}
			}
			if others {
				ungrouped.removeHost(h)
			}
			continue
		}
		if len(h.groups) == 0 || (len(h.groups) == 1 && h.groups[0] == all) {
			if err := d.addChild("ungrouped", name); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeHost removes h from g and drops g from the host's groups. The ancestors g brought are kept
// when another of the host's groups still sits below them, and all is always kept.
func (g *invGroup) removeHost(h *invHost) {
	if !g.hostNames[h.name] {
		return
	}
	delete(g.hostNames, h.name)
	for i, have := range g.hosts {
		if have == h {
			g.hosts = append(g.hosts[:i], g.hosts[i+1:]...)
			break
		}
	}
	h.removeGroup(g)
}

// removeGroup drops g from the host's groups, and each ancestor of g no other group of the host
// sits below, except all.
func (h *invHost) removeGroup(g *invGroup) {
	idx := -1
	for i, have := range h.groups {
		if have == g {
			idx = i
		}
	}
	if idx < 0 {
		return
	}
	h.groups = append(h.groups[:idx], h.groups[idx+1:]...)
	for _, old := range g.ancestors() {
		if old.name == "all" {
			continue
		}
		kept := false
		for _, other := range h.groups {
			if containsGroup(other.ancestors(), old) {
				kept = true
				break
			}
		}
		if !kept {
			h.removeGroup(old)
		}
	}
}

// reservedVars are the names ansible-inventory --list leaves out of a host's variables in any
// supported release: the internal variables each release strips. A union is used so that the
// native engine and every release agree; a variable by one of these names is not one an inventory
// can usefully set, since Ansible sets it itself when a play runs.
var reservedVars = map[string]bool{
	"ansible_async_path": true, "ansible_collection_name": true, "ansible_config_file": true,
	"ansible_dependent_role_names": true, "ansible_diff_mode": true, "ansible_facts": true,
	"ansible_forks": true, "ansible_inventory_sources": true, "ansible_limit": true,
	"ansible_play_batch": true, "ansible_play_hosts": true, "ansible_play_hosts_all": true,
	"ansible_play_role_names": true, "ansible_playbook_python": true, "ansible_role_name": true,
	"ansible_role_names": true, "ansible_run_tags": true, "ansible_skip_tags": true,
	"ansible_verbosity": true, "ansible_version": true, "inventory_dir": true,
	"inventory_file": true, "inventory_hostname": true, "inventory_hostname_short": true,
	"groups": true, "group_names": true, "hostvars": true, "omit": true, "playbook_dir": true,
	"play_hosts": true, "role_name": true, "role_names": true, "role_path": true, "role_uuid": true,
}

// hostVars returns the host's variables as ansible-inventory --list resolves them: all's variables,
// then every other group the host belongs to ordered by depth, then priority, then name, each
// replacing what came before, then the host's own, with the reserved names left out. converted
// caches each variable mapping in its printed form, since every host of a group shares the group's.
func (d *invData) hostVars(h *invHost, converted map[*varMap]map[string]any) (map[string]any, error) {
	out := map[string]any{}
	merge := func(m *varMap) error {
		c, ok := converted[m]
		if !ok {
			c = make(map[string]any, len(m.keys))
			for _, k := range m.keys {
				v, err := listingValue(m.values[k])
				if err != nil {
					return err
				}
				c[k] = v
			}
			converted[m] = c
		}
		for k, v := range c {
			out[k] = v
		}
		return nil
	}
	if err := merge(d.groups["all"].vars); err != nil {
		return nil, err
	}
	groups := make([]*invGroup, 0, len(h.groups))
	for _, g := range h.groups {
		if g.name != "all" {
			groups = append(groups, g)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.name < b.name
	})
	for _, g := range groups {
		if err := merge(g.vars); err != nil {
			return nil, err
		}
	}
	if err := merge(h.vars); err != nil {
		return nil, err
	}
	for k := range out {
		if reservedVars[k] {
			delete(out, k)
		}
	}
	return out, nil
}

// ownVars returns the variables set on the host itself, with the reserved names left out.
func (d *invData) ownVars(h *invHost) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range h.vars.keys {
		if reservedVars[k] {
			continue
		}
		v, err := listingValue(h.vars.values[k])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// listing returns the inventory as ansible-inventory --list prints it: every group below all with
// its direct hosts and child groups, leaving out a group with neither, and every host's resolved
// variables.
func (d *invData) listing() (*Listing, error) {
	l := &Listing{groups: map[string]*listedGroup{}, hostVars: map[string]map[string]any{}}
	for _, name := range d.groupOrder {
		g := d.groups[name]
		if name == "all" {
			continue
		}
		lg := &listedGroup{}
		for _, h := range g.hosts {
			lg.Hosts = append(lg.Hosts, h.name)
		}
		for _, c := range g.children {
			lg.Children = append(lg.Children, c.name)
		}
		if len(lg.Hosts) == 0 && len(lg.Children) == 0 {
			continue
		}
		l.groups[name] = lg
	}
	converted := map[*varMap]map[string]any{}
	for _, name := range d.hostOrder {
		vars, err := d.hostVars(d.hosts[name], converted)
		if err != nil {
			return nil, err
		}
		if len(vars) > 0 {
			l.hostVars[name] = vars
		}
	}
	return l, nil
}
