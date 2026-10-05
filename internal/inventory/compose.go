package inventory

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// constructedKeys are the constructed plugin options a constructed inventory may set. They are the
// ones AWX documents for a constructed inventory and writes into source_vars. Anything else is
// refused rather than handed to ansible-inventory, because an option this list does not name is an
// option nobody reviewed for what it can reach.
var constructedKeys = []string{
	"plugin", "strict", "compose", "groups", "keyed_groups", "leading_separator",
	"use_vars_plugins",
}

// keyedGroupKeys are the fields one keyed_groups entry may carry.
var keyedGroupKeys = []string{
	"key", "prefix", "separator", "parent_group", "default_value", "trailing_separator",
}

// constructedPlugins are the names the plugin option may carry.
var constructedPlugins = []string{"constructed", "ansible.builtin.constructed"}

// lookupMarkers are the Jinja calls that reach outside the inventory: a lookup can read a file or
// run a command on the server. The constructed plugin's expressions only need host variables, so an
// option holding one is refused.
var lookupMarkers = []string{"lookup(", "query(", "q("}

// maxSourceVarsLen bounds the plugin options so a pasted or imported document stays a config.
const maxSourceVarsLen = 64 << 10

// ConstructedConfig validates a constructed inventory's plugin options and returns the plugin
// config file ansible-inventory reads, with the plugin named. Empty options are a constructed
// inventory that merges its inputs and builds nothing, which is valid.
func ConstructedConfig(sourceVars string) ([]byte, error) {
	if len(sourceVars) > maxSourceVarsLen {
		return nil, fmt.Errorf("%w: they are longer than %d bytes", ErrSourceVars, maxSourceVarsLen)
	}
	doc := map[string]any{}
	if strings.TrimSpace(sourceVars) != "" {
		dec := yaml.NewDecoder(strings.NewReader(sourceVars))
		if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: they are not a YAML mapping: %w", ErrSourceVars, err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: they hold more than one YAML document", ErrSourceVars)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	for key, value := range doc {
		if !slices.Contains(constructedKeys, key) {
			return nil, fmt.Errorf("%w: %q is not a constructed plugin option this accepts; use %s",
				ErrSourceVars, key, strings.Join(constructedKeys[1:], ", "))
		}
		if err := checkOption(key, value); err != nil {
			return nil, err
		}
	}
	doc["plugin"] = "constructed"
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceVars, err)
	}
	return out, nil
}

// checkOption validates the shape of one plugin option.
func checkOption(key string, value any) error {
	switch key {
	case "plugin":
		name, ok := value.(string)
		if !ok || !slices.Contains(constructedPlugins, name) {
			return fmt.Errorf("%w: plugin must be constructed", ErrSourceVars)
		}
	case "strict", "leading_separator", "use_vars_plugins":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%w: %s must be true or false", ErrSourceVars, key)
		}
	case "compose", "groups":
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s must map names to expressions", ErrSourceVars, key)
		}
		for name, expr := range m {
			if err := checkExpression(key+"."+name, expr); err != nil {
				return err
			}
		}
	case "keyed_groups":
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%w: keyed_groups must be a list", ErrSourceVars)
		}
		for i, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: keyed_groups entry %d must be a mapping", ErrSourceVars, i)
			}
			if _, ok := entry["key"]; !ok {
				return fmt.Errorf("%w: keyed_groups entry %d needs a key", ErrSourceVars, i)
			}
			for field, v := range entry {
				if !slices.Contains(keyedGroupKeys, field) {
					return fmt.Errorf("%w: keyed_groups entry %d has %q, which is not one of %s",
						ErrSourceVars, i, field, strings.Join(keyedGroupKeys, ", "))
				}
				if err := checkExpression(fmt.Sprintf("keyed_groups[%d].%s", i, field), v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkExpression refuses an expression that is not a scalar or that calls a lookup.
func checkExpression(where string, expr any) error {
	switch v := expr.(type) {
	case string:
		compact := strings.ReplaceAll(strings.ToLower(v), " ", "")
		for _, marker := range lookupMarkers {
			if strings.Contains(compact, marker) {
				return fmt.Errorf("%w: %s calls a lookup, which reads outside the inventory",
					ErrSourceVars, where)
			}
		}
	case bool, int, float64, nil:
	default:
		return fmt.Errorf("%w: %s must be an expression", ErrSourceVars, where)
	}
	return nil
}

// Validate checks that an inventory's kind and the fields that belong to it agree: a static
// inventory carries no composition, a smart one a filter that parses, and a constructed one at
// least one input and options the plugin accepts. A composed inventory holds no content, since its
// hosts come from its inputs at every launch.
func Validate(i *Inventory) error {
	if i == nil {
		return fmt.Errorf("%w: no inventory", ErrComposition)
	}
	switch i.Kind {
	case KindStatic:
		if i.HostFilter != "" || len(i.InputIDs) > 0 || i.SourceVars != "" || i.Limit != "" {
			return fmt.Errorf("%w: a host filter, inputs, plugin options, and a limit belong to a "+
				"smart or constructed inventory", ErrComposition)
		}
		return nil
	case KindSmart:
		if len(i.InputIDs) > 0 || i.SourceVars != "" || i.Limit != "" {
			return fmt.Errorf("%w: a smart inventory is a host filter alone; inputs, plugin "+
				"options, and a limit belong to a constructed inventory", ErrComposition)
		}
		if _, err := ParseHostFilter(i.HostFilter); err != nil {
			return err
		}
	case KindConstructed:
		if i.HostFilter != "" {
			return fmt.Errorf("%w: a constructed inventory narrows with a limit, not a host "+
				"filter", ErrComposition)
		}
		if len(i.InputIDs) == 0 {
			return fmt.Errorf("%w: a constructed inventory needs at least one input inventory",
				ErrComposition)
		}
		seen := map[string]bool{}
		for _, id := range i.InputIDs {
			switch {
			case id == "":
				return fmt.Errorf("%w: an input inventory id is empty", ErrComposition)
			case id == i.ID:
				return fmt.Errorf("%w: a constructed inventory cannot be its own input",
					ErrComposition)
			case seen[id]:
				return fmt.Errorf("%w: input inventory %s is listed twice", ErrComposition, id)
			}
			seen[id] = true
		}
		if _, err := ConstructedConfig(i.SourceVars); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: kind must be empty, smart, or constructed, not %q", ErrComposition,
			i.Kind)
	}
	if strings.TrimSpace(i.Content) != "" || (i.ContentSource != "" && i.ContentSource != "local") {
		return fmt.Errorf("%w: a %s inventory takes its hosts from other inventories, so it holds "+
			"no content of its own", ErrComposition, i.Kind)
	}
	return nil
}
