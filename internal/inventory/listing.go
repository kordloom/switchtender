package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
)

// Listing is an inventory as ansible-inventory --list prints it: groups naming their hosts and
// child groups, and every host's resolved variables under _meta.hostvars.
//
// It is how a composed inventory reads its inputs. Ansible parses every inventory format there is,
// INI, YAML, the JSON a source keeps, and folds group variables into each host, so reading its
// output rather than the stored text keeps one parser of inventory files in the product, and it
// is the one ansible-playbook will use on the result.
type Listing struct {
	// groups maps a group name to its members, without all.
	groups map[string]*listedGroup
	// hostVars maps a host to its resolved variables.
	hostVars map[string]map[string]any
}

// listedGroup is one group in a listing.
type listedGroup struct {
	// Hosts are the group's direct members.
	Hosts []string `json:"hosts"`
	// Children are the groups nested under this one.
	Children []string `json:"children"`
}

// ParseListing reads the JSON ansible-inventory --list prints. Numbers keep their literal form, so
// a large integer in a host variable reaches the composed inventory exactly as it was written.
func ParseListing(data []byte) (*Listing, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrListing, err)
	}
	l := &Listing{groups: map[string]*listedGroup{}, hostVars: map[string]map[string]any{}}
	for name, raw := range doc {
		if name == "_meta" {
			var meta struct {
				// HostVars are the resolved variables of each host that has any.
				HostVars map[string]map[string]any `json:"hostvars"`
			}
			mdec := json.NewDecoder(bytes.NewReader(raw))
			mdec.UseNumber()
			if err := mdec.Decode(&meta); err != nil {
				return nil, fmt.Errorf("%w: _meta: %w", ErrListing, err)
			}
			for h, vars := range meta.HostVars {
				l.hostVars[h] = vars
			}
			continue
		}
		var g listedGroup
		// A group may carry only children, and all usually does, so a shape without hosts is
		// ordinary rather than an error.
		if err := json.Unmarshal(raw, &g); err != nil {
			continue
		}
		if name == "all" {
			continue
		}
		l.groups[name] = &g
	}
	return l, nil
}

// Hosts returns every host the listing names, sorted. A host is read from the groups as well as
// from hostvars, because Ansible writes a host into hostvars only when it has variables.
func (l *Listing) Hosts() []string {
	seen := map[string]bool{}
	for h := range l.hostVars {
		seen[h] = true
	}
	for _, g := range l.groups {
		for _, h := range g.Hosts {
			seen[h] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Groups returns the groups host is a direct member of, sorted, leaving out ungrouped, which is
// where Ansible files a host that belongs to nothing.
func (l *Listing) Groups(host string) []string {
	var out []string
	for name, g := range l.groups {
		if name == "ungrouped" {
			continue
		}
		if slices.Contains(g.Hosts, host) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Vars returns host's resolved variables, nil when it has none.
func (l *Listing) Vars(host string) map[string]any {
	return l.hostVars[host]
}

// Restrict returns a copy of the listing holding only the hosts in keep. Groups left with neither
// hosts nor children are dropped, so the inventory a run receives names nothing it cannot reach.
func (l *Listing) Restrict(keep map[string]bool) *Listing {
	out := &Listing{groups: map[string]*listedGroup{}, hostVars: map[string]map[string]any{}}
	for h, vars := range l.hostVars {
		if keep[h] {
			out.hostVars[h] = vars
		}
	}
	for name, g := range l.groups {
		cp := &listedGroup{Children: slices.Clone(g.Children)}
		for _, h := range g.Hosts {
			if keep[h] {
				cp.Hosts = append(cp.Hosts, h)
			}
		}
		if len(cp.Hosts) == 0 && len(cp.Children) == 0 {
			continue
		}
		out.groups[name] = cp
	}
	return out
}

// NewHostListing builds a listing of ungrouped hosts with their variables, which is the shape of a
// smart inventory: AWX gives a smart inventory its matching hosts and none of their groups.
func NewHostListing(hosts []string, vars map[string]map[string]any) *Listing {
	l := &Listing{groups: map[string]*listedGroup{}, hostVars: map[string]map[string]any{}}
	g := &listedGroup{}
	for _, h := range hosts {
		g.Hosts = append(g.Hosts, h)
		if v := vars[h]; len(v) > 0 {
			l.hostVars[h] = v
		}
	}
	if len(g.Hosts) > 0 {
		l.groups["ungrouped"] = g
	}
	return l
}

// Static renders the listing as an inventory document Ansible's YAML plugin reads, written as JSON:
// each group with its hosts and children, and each host's variables written once, on its first
// appearance. A host that belongs to no group is filed under ungrouped, so it is not lost.
//
// The variables are already resolved, group variables included, so they are written as host
// variables and no group carries any. The result therefore reads the same to ansible-playbook as
// the listing it came from.
func (l *Listing) Static() ([]byte, error) {
	out := map[string]any{}
	written := map[string]bool{}
	grouped := map[string]bool{}
	names := slices.Sorted(maps.Keys(l.groups))
	for _, name := range names {
		for _, h := range l.groups[name].Hosts {
			grouped[h] = true
		}
	}
	if extra := l.ungroupedExtra(grouped); len(extra) > 0 {
		g := &listedGroup{}
		if have := l.groups["ungrouped"]; have != nil {
			g.Hosts, g.Children = slices.Clone(have.Hosts), slices.Clone(have.Children)
		}
		g.Hosts = append(g.Hosts, extra...)
		l = l.withGroup("ungrouped", g)
		names = slices.Sorted(maps.Keys(l.groups))
	}
	for _, name := range names {
		g := l.groups[name]
		entry := map[string]any{}
		if len(g.Hosts) > 0 {
			hosts := map[string]any{}
			for _, h := range g.Hosts {
				if vars := l.hostVars[h]; len(vars) > 0 && !written[h] {
					hosts[h] = vars
					written[h] = true
					continue
				}
				hosts[h] = nil
			}
			entry["hosts"] = hosts
		}
		if len(g.Children) > 0 {
			children := map[string]any{}
			for _, c := range g.Children {
				children[c] = nil
			}
			entry["children"] = children
		}
		out[name] = entry
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrListing, err)
	}
	return buf.Bytes(), nil
}

// ungroupedExtra returns the hosts with variables that no group names, sorted.
func (l *Listing) ungroupedExtra(grouped map[string]bool) []string {
	var extra []string
	for h := range l.hostVars {
		if !grouped[h] {
			extra = append(extra, h)
		}
	}
	sort.Strings(extra)
	return extra
}

// withGroup returns a shallow copy of the listing with one more group, leaving the receiver alone.
func (l *Listing) withGroup(name string, g *listedGroup) *Listing {
	cp := &Listing{groups: maps.Clone(l.groups), hostVars: l.hostVars}
	cp.groups[name] = g
	return cp
}

// ListJSON renders the listing the way ansible-inventory --list prints it: all with its child
// groups, every other group with its hosts and children as lists, and each host's variables under
// _meta.hostvars. It is the inverse of ParseListing.
func (l *Listing) ListJSON() ([]byte, error) {
	out := map[string]any{}
	all := map[string]any{}
	var top []string
	childOf := map[string]bool{}
	for _, g := range l.groups {
		for _, c := range g.Children {
			childOf[c] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(l.groups)) {
		g := l.groups[name]
		entry := map[string]any{}
		if len(g.Hosts) > 0 {
			entry["hosts"] = g.Hosts
		}
		if len(g.Children) > 0 {
			entry["children"] = g.Children
		}
		out[name] = entry
		if !childOf[name] {
			top = append(top, name)
		}
	}
	if len(top) > 0 {
		all["children"] = top
	}
	out["all"] = all
	out["_meta"] = map[string]any{"hostvars": l.hostVars}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrListing, err)
	}
	return buf.Bytes(), nil
}
