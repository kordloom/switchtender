package server

import "testing"

// FuzzParseProposedRun feeds arbitrary model replies to the proposal parser to prove it never
// panics and never reports a usable run that is missing its tool.
func FuzzParseProposedRun(f *testing.F) {
	f.Add(`{"tool":"bash","command":"echo hi"}`)
	f.Add("```json\n{\"tool\":\"ansible\",\"playbook\":\"p.yml\"}\n```")
	f.Add("prose then {\"tool\":\"go\",\"command\":\"package main\"} trailing")
	f.Add("not json at all")
	f.Fuzz(func(t *testing.T, s string) {
		p, ok := parseProposedRun(s)
		if ok && p.Playbook == "" && p.Command == "" {
			t.Fatalf("parseProposedRun returned ok with neither playbook nor command for %q", s)
		}
	})
}
