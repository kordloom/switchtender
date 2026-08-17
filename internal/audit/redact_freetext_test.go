package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalRedactedScansInsideStringValues covers a secret that reached the one artifact meant to
// leave the building.
//
// The redactor decided by key name: a value under a name like password went, and everything else
// stayed. A run's variables are ordinary strings, and an ordinary string can be a command line, and a
// command line can hold a password. So an extra var named deploy_cmd whose value was
// "psql 'host=db password=hunter2' ..." kept its password, and that spec is disclosed in the signed
// receipt a customer hands an outside auditor.
//
// The inventory redactor already read these assignments, and its own comment names this exact threat,
// so the same string was masked in an inventory and shipped verbatim in a receipt. Both now read it the
// same way, through one implementation.
func TestCanonicalRedactedScansInsideStringValues(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		// Name says what shape the secret arrived in.
		Name string
		// In is the record before redaction.
		In map[string]any
		// Secret must not survive.
		Secret string
		// Keep is text that must survive, so the redaction stays narrow.
		Keep string
	}{{ // Test 0: An assignment inside a command line, under a name nothing would call secret.
		Name: "inside a command line",
		In: map[string]any{"extra_vars": map[string]any{
			"deploy_cmd": "psql 'host=db password=hunter2' -c 'select 1'",
		}},
		Secret: "hunter2", Keep: "psql",
	}, { // Test 1: A YAML-style assignment, the other form these arrive in.
		Name: "yaml style",
		In: map[string]any{"extra_vars": map[string]any{
			"notes": "api_token: tok_abcdef123456",
		}},
		Secret: "tok_abcdef123456", Keep: "api_token",
	}, { // Test 2: Nested inside a list, since variables hold lists.
		Name: "inside a list",
		In: map[string]any{"extra_vars": map[string]any{
			"steps": []any{"echo hi", "mysql --password=Pr0dSecret"},
		}},
		Secret: "Pr0dSecret", Keep: "echo hi",
	}, { // Test 3: A value under a secret-sounding key is still handled, which was already true.
		Name:   "under a secret key",
		In:     map[string]any{"extra_vars": map[string]any{"db_password": "hunter2"}},
		Secret: "hunter2", Keep: "db_password",
	}, { // Test 4: An ordinary assignment nobody would call secret is left alone, so the redaction does
		// not blank out a spec a reader needs.
		Name: "an ordinary assignment survives",
		In: map[string]any{"extra_vars": map[string]any{
			"release": "version=1.2.3",
		}},
		Secret: "", Keep: "1.2.3",
	}} {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.In)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			got := string(CanonicalRedacted(raw))
			if test.Secret != "" && strings.Contains(got, test.Secret) {
				t.Errorf("the redacted record still carries %q, so it would ship in the signed receipt "+
					"a customer hands an auditor:\n%s", test.Secret, got)
			}
			if !strings.Contains(got, test.Keep) {
				t.Errorf("the redaction removed %q, which is not a secret:\n%s", test.Keep, got)
			}
		})
	}
}
