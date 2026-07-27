package server

import (
	"fmt"
	"testing"
)

// TestTemplateToolError checks template input validation per tool, including that any tool, not
// just Ansible, may pin an execution image now that the container runner plans all seven.
func TestTemplateToolError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Req  createTemplateRequest
		Want string
	}{{ // Test 0: Ansible with a playbook is valid.
		Name: "ansible ok",
		Req:  createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml"},
		Want: "",
	}, { // Test 1: Ansible without a playbook is rejected.
		Name: "ansible needs playbook",
		Req:  createTemplateRequest{Name: "t", Tool: "ansible"},
		Want: "playbook is required",
	}, { // Test 2: A non-Ansible tool with a command is valid.
		Name: "bash ok",
		Req:  createTemplateRequest{Name: "t", Tool: "bash", Command: "echo hi"},
		Want: "",
	}, { // Test 3: A non-Ansible tool with an image is valid, the container gate is gone.
		Name: "terraform in a container",
		Req:  createTemplateRequest{Name: "t", Tool: "terraform", Command: "apply", Image: "ghcr.io/acme/tf:1"},
		Want: "",
	}, { // Test 4: A non-Ansible tool still needs a command.
		Name: "python needs command",
		Req:  createTemplateRequest{Name: "t", Tool: "python", Image: "ghcr.io/acme/py:3"},
		Want: "command is required for the python tool",
	}, { // Test 5: An unknown tool is rejected.
		Name: "unknown tool",
		Req:  createTemplateRequest{Name: "t", Tool: "make", Command: "all"},
		Want: "tool must be ansible, bash, terraform, opentofu, python, powershell, or go",
	}, { // Test 6: A missing name is rejected first.
		Name: "name required",
		Req:  createTemplateRequest{Tool: "ansible", Playbook: "site.yml"},
		Want: "name is required",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := templateToolError(test.Req); got != test.Want {
				t.Errorf("templateToolError() = %q, want %q", got, test.Want)
			}
		})
	}
}
