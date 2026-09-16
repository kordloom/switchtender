package event

import (
	"fmt"
	"strings"
	"testing"
)

// TestAYamlBooleanTaskNameDoesNotDiscardTheEvent covers a silent, total data loss found by running
// a real playbook against a real fleet.
//
// YAML reads an unquoted True, No, Off, or 1.0 as a boolean or a number, so a task written as
// "- name: No" is named with a boolean and the callback emitted it as one. Decoding rejected the
// line, and a rejected line is a dropped event, so every event for that task vanished: the run's
// host-by-task matrix lost those rows and the only trace was a parse error in the server's log
// that no user ever sees. The name reads oddly in the interface; losing the task does not read at
// all.
func TestAYamlBooleanTaskNameDoesNotDiscardTheEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Raw      string
		WantTask string
	}{
		// Test 0: "- name: No" in a playbook, which is how Norway and a plain refusal both arrive.
		{Raw: `{"type":"task_start","ts":1,"play":"p","task":false}`, WantTask: "false"},
		{Raw: `{"type":"task_start","ts":1,"play":"p","task":true}`, WantTask: "true"},   // Test 1.
		{Raw: `{"type":"task_start","ts":1,"play":"p","task":1.0}`, WantTask: "1.0"},     // Test 2.
		{Raw: `{"type":"task_start","ts":1,"play":"p","task":"real"}`, WantTask: "real"}, // Test 3.
		{Raw: `{"type":"task_start","ts":1,"play":"p"}`, WantTask: ""},                   // Test 4: absent.
		{Raw: `{"type":"task_start","ts":1,"play":"p","task":null}`, WantTask: ""},       // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			evs, err := Parse(strings.NewReader(test.Raw))
			if err != nil {
				t.Fatalf("Parse() error = %v, want the event kept", err)
			}
			if len(evs) != 1 {
				t.Fatalf("Parse() returned %d events, want 1. The line was dropped, and with it "+
					"every record of that task", len(evs))
			}
			if evs[0].Task != test.WantTask {
				t.Errorf("task = %q, want %q", evs[0].Task, test.WantTask)
			}
		})
	}
}
