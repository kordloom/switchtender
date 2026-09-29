package server

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRunReadsScrubInlineSecretsBelowAdmin covers the surface an outside auditor is given.
//
// A run carries its command and its launch variables verbatim. The receipt scrubs them, the dossier
// scrubs them, the change register scrubs them, the webhook notification scrubs them, and the
// inventory list scrubs its own equivalents, so six surfaces agreed and the seventh did not: GET
// /v1/runs served every run's script body and every launch variable in the clear to the read-only
// viewer role, which is precisely the role handed to someone outside the team.
//
// Only the secret-shaped assignments are masked, never the whole command, so an operator can still
// read what a run did. An admin sees the original, the same reasoning redactInventories records:
// they hold every credential on the install already.
func TestRunReadsScrubInlineSecretsBelowAdmin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which role is reading.
		Name string
		// Role is the caller's role.
		Role user.Role
		// WantCommand is the command as that role should receive it.
		WantCommand string
		// WantVar is the db_password launch variable as that role should receive it.
		WantVar string
	}{{ // Test 0: A viewer, the role given to an auditor, sees no secret.
		Name: "viewer", Role: user.RoleViewer,
		WantCommand: `psql "host=db password=[redacted]"`, WantVar: "[redacted]",
	}, { // Test 1: An operator reads the command but not the secret in it.
		Name: "operator", Role: user.RoleOperator,
		WantCommand: `psql "host=db password=[redacted]"`, WantVar: "[redacted]",
	}, { // Test 2: An admin holds every credential already, so redacting only obstructs them.
		Name: "admin", Role: user.RoleAdmin,
		WantCommand: `psql "host=db password=hunter2"`, WantVar: "hunter2",
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			stored := &run.Run{
				ID: "run_1", Tool: "bash",
				Command:   `psql "host=db password=hunter2"`,
				ExtraVars: map[string]any{"db_password": "hunter2", "env": "prod"},
			}
			ctx := context.WithValue(context.Background(), actorKey{},
				Actor{UserID: "u1", Role: test.Role})

			got := scrubbedRun(ctx, stored)
			if diff := cmp.Diff(test.WantCommand, got.Command); diff != "" {
				t.Errorf("%s: command mismatch (-want +got):\n%s", test.Name, diff)
			}
			if diff := cmp.Diff(test.WantVar, got.ExtraVars["db_password"]); diff != "" {
				t.Errorf("%s: db_password mismatch (-want +got):\n%s", test.Name, diff)
			}
			if got.ExtraVars["env"] != "prod" {
				t.Errorf("%s: a variable that is not a secret was altered: %v",
					test.Name, got.ExtraVars["env"])
			}
			// The stored object must never be mutated: a memory-backed store hands back live
			// pointers, so a redaction that wrote through would destroy the real command.
			if stored.Command != `psql "host=db password=hunter2"` {
				t.Errorf("%s: the stored run was mutated: %q", test.Name, stored.Command)
			}
			if stored.ExtraVars["db_password"] != "hunter2" {
				t.Errorf("%s: the stored launch variables were mutated: %v",
					test.Name, stored.ExtraVars)
			}
		})
	}
}
