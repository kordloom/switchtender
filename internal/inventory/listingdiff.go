package inventory

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kordloom/switchtender/internal/util"
)

// maxDifferences bounds how many differences a comparison lists, so a refusal stays readable when
// two views disagree about a whole fleet.
const maxDifferences = 25

// DiffListings compares the native engine's view of an inventory with Ansible's and returns every
// difference in hosts, group memberships, child groups, and host variables, each as a sentence. The
// order of hosts and groups is not compared, since nothing a run does depends on it once the set is
// fixed. A secret-looking variable is named and never quoted. The list is empty when they agree.
func DiffListings(native, ansible *Listing) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf(format, args...))
	}
	nativeHosts, ansibleHosts := setOf(native.Hosts()), setOf(ansible.Hosts())
	for _, h := range sortedKeys(nativeHosts) {
		if !ansibleHosts[h] {
			add("host %s is in the native view and not in Ansible's", h)
		}
	}
	for _, h := range sortedKeys(ansibleHosts) {
		if !nativeHosts[h] {
			add("host %s is in Ansible's view and not in the native one", h)
		}
	}
	names := setOf(sortedKeys(native.groups))
	for name := range ansible.groups {
		names[name] = true
	}
	for _, name := range sortedKeys(names) {
		ng, ag := native.groups[name], ansible.groups[name]
		switch {
		case ng == nil:
			add("group %s is in Ansible's view and not in the native one", name)
			continue
		case ag == nil:
			add("group %s is in the native view and not in Ansible's", name)
			continue
		}
		diffMembers(name, "host", ng.Hosts, ag.Hosts, add)
		diffMembers(name, "child group", ng.Children, ag.Children, add)
	}
	for _, h := range sortedKeys(nativeHosts) {
		if !ansibleHosts[h] {
			continue
		}
		diffVars(h, native.hostVars[h], ansible.hostVars[h], add)
	}
	if len(out) > maxDifferences {
		more := len(out) - maxDifferences
		out = append(out[:maxDifferences], fmt.Sprintf("and %d more", more))
	}
	return out
}

// setOf returns the members of list as a set.
func setOf(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out
}

// diffMembers reports the members of one group the two views do not share.
func diffMembers(group, noun string, native, ansible []string, add func(string, ...any)) {
	n, a := setOf(native), setOf(ansible)
	for _, m := range sortedKeys(n) {
		if !a[m] {
			add("group %s has %s %s in the native view and not in Ansible's", group, noun, m)
		}
	}
	for _, m := range sortedKeys(a) {
		if !n[m] {
			add("group %s has %s %s in Ansible's view and not in the native one", group, noun, m)
		}
	}
}

// diffVars reports the variables of one host the two views do not agree on.
func diffVars(host string, native, ansible map[string]any, add func(string, ...any)) {
	keys := setOf(sortedKeys(native))
	for k := range ansible {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		// A reserved name is one ansible-inventory keeps in some releases and drops in others, and
		// one Ansible overrides when a play runs, so it is not compared.
		if reservedVars[k] {
			continue
		}
		nv, inNative := native[k]
		av, inAnsible := ansible[k]
		secret := util.SecretKey(k)
		switch {
		case !inAnsible:
			add("host %s variable %s is set in the native view and not in Ansible's", host, k)
		case !inNative:
			add("host %s variable %s is set in Ansible's view and not in the native one", host, k)
		case !sameValue(nv, av):
			if secret {
				add("host %s variable %s differs (the value is secret and not shown)", host, k)
				continue
			}
			add("host %s variable %s is %s in the native view and %s in Ansible's", host, k,
				describeValue(nv), describeValue(av))
		}
	}
}

// sameValue reports whether two listing values are the same value of the same type. Numbers are
// compared by the text Python printed for them, which tells an int from a float.
func sameValue(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, sameValue)
	}
	return a == b
}

// describeValue renders a listing value for a difference: its JSON with its type named, secrets in
// nested mappings masked, and long values clipped.
func describeValue(v any) string {
	if m, ok := v.(map[string]any); ok {
		v = MaskSecrets(m)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "an unreadable value"
	}
	text := util.Clip(string(b), 120)
	return text + " (" + valueKind(v) + ")"
}

// valueKind names a listing value's type the way Python would.
func valueKind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case json.Number:
		if strings.ContainsAny(string(t), ".eEn") {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	case map[string]any:
		if _, ok := t["__ansible_unsafe"]; ok && len(t) == 1 {
			return "unsafe string"
		}
		return "mapping"
	}
	return "value"
}
