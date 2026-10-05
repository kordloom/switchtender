package dispatch

import (
	"bytes"
	"encoding/json"
)

// staticFromDump converts ansible-inventory --list output into a static inventory document that
// ansible-playbook can read from a file. The dynamic format lists group children and hosts as
// arrays; the static format the yaml and json plugins accept uses dictionaries and folds each
// host's vars in from the _meta block.
//
// Numbers keep the text Ansible printed. Decoded as float64, a float such as 4.0 was written back
// as 4, which Ansible then reads as an int, and an int past 2^53 lost its last digits, so the
// stored inventory and every play run from it held different values from the ones the source
// produced.
func staticFromDump(dump []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(dump, &doc); err != nil {
		return nil, err
	}

	hostVars := map[string]map[string]any{}
	if meta, ok := doc["_meta"]; ok {
		var m struct {
			HostVars map[string]map[string]any `json:"hostvars"`
		}
		dec := json.NewDecoder(bytes.NewReader(meta))
		dec.UseNumber()
		if err := dec.Decode(&m); err == nil {
			hostVars = m.HostVars
		}
	}

	out := map[string]any{}
	for name, raw := range doc {
		if name == "_meta" || name == "all" {
			continue
		}
		var group struct {
			Hosts    []string       `json:"hosts"`
			Children []string       `json:"children"`
			Vars     map[string]any `json:"vars"`
		}
		gdec := json.NewDecoder(bytes.NewReader(raw))
		gdec.UseNumber()
		if err := gdec.Decode(&group); err != nil {
			continue
		}
		entry := map[string]any{}
		if len(group.Hosts) > 0 {
			hosts := map[string]any{}
			for _, h := range group.Hosts {
				if vars := hostVars[h]; len(vars) > 0 {
					hosts[h] = vars
				} else {
					hosts[h] = nil
				}
			}
			entry["hosts"] = hosts
		}
		if len(group.Children) > 0 {
			children := map[string]any{}
			for _, c := range group.Children {
				children[c] = nil
			}
			entry["children"] = children
		}
		if len(group.Vars) > 0 {
			entry["vars"] = group.Vars
		}
		out[name] = entry
	}

	return json.MarshalIndent(out, "", "  ")
}
