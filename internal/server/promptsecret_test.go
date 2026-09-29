package server

import (
	"strings"
	"testing"
)

// TestPromptCommandScrubsInlineSecrets covers the one field that left the host unredacted.
//
// redactForExternal blanks Command from every webhook and plugin notification, on the stated grounds
// that it holds the raw script body and can embed inline secrets or sensitive arguments. The AI
// prompts wrote that same field straight into a payload POSTed to a third-party API, so a bash run
// whose script carried a bearer token or a database URL handed it over the moment somebody clicked
// Explain. The log tail in the same prompt was already masked, which is what left this as the only
// unredacted secret-bearing field in the request.
func TestPromptCommandScrubsInlineSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Command string
		Gone    string
		Kept    string
	}{{ // Test 0: A bearer token in a curl header.
		Name:    "bearer token",
		Command: `curl -H "Authorization: Bearer sk-prod-abc123xyz" https://api.example.com/deploy`,
		Gone:    "sk-prod-abc123xyz", Kept: "curl",
	}, { // Test 1: A password in a connection string assignment.
		Name:    "password assignment",
		Command: "export DB_PASSWORD=hunter2\npsql -c 'select 1'",
		Gone:    "hunter2", Kept: "psql",
	}, { // Test 2: An API key assigned inline.
		Name:    "api key",
		Command: "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG deploy.sh",
		Gone:    "wJalrXUtnFEMI/K7MDENG", Kept: "deploy.sh",
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got := promptCommand(test.Command, 2000)
			if strings.Contains(got, test.Gone) {
				t.Errorf("%s: the prompt still carries %q off the host:\n%s",
					test.Name, test.Gone, got)
			}
			if !strings.Contains(got, test.Kept) {
				t.Errorf("%s: the prompt lost %q, so the scrub gutted what is being explained:\n%s",
					test.Name, test.Kept, got)
			}
		})
	}

	// An ordinary script is unchanged, or the feature is worse for everybody to protect a few.
	plain := "ansible-playbook site.yml --limit web --check"
	if got := promptCommand(plain, 2000); got != plain {
		t.Errorf("an ordinary command was altered: %q became %q", plain, got)
	}
}
