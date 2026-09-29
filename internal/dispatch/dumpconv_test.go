package dispatch

import (
	"encoding/json"
	"testing"
)

func TestStaticFromDump(t *testing.T) {
	t.Parallel()
	dump := []byte(`{
		"all": {"children": ["dyn", "ungrouped"]},
		"dyn": {"hosts": ["a", "b"]},
		"_meta": {"hostvars": {"a": {"ansible_host": "10.0.0.1"}, "b": {}}}
	}`)
	got, err := staticFromDump(dump)
	if err != nil {
		t.Fatalf("staticFromDump() error = %v", err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("result is not valid json: %v", err)
	}
	dyn, ok := out["dyn"]
	if !ok {
		t.Fatalf("group dyn missing from %s", got)
	}
	hosts, ok := dyn["hosts"].(map[string]any)
	if !ok || len(hosts) != 2 {
		t.Fatalf("dyn hosts = %v, want two host keys", dyn["hosts"])
	}
	av, ok := hosts["a"].(map[string]any)
	if !ok || av["ansible_host"] != "10.0.0.1" {
		t.Errorf("host a vars = %v, want ansible_host folded in", hosts["a"])
	}
	if _, ok := out["_meta"]; ok {
		t.Error("static output should not carry _meta")
	}
}
