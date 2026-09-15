package util

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestRedactAssignmentsNested pins that a secret is still masked when it is joined onto another
// assignment's value with no separating space, the case the value scan exists for, and that ordinary
// space and newline separated assignments are unchanged.
func TestRedactAssignmentsNested(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Want string
	}{{ // Test 0: A secret joined onto a non-secret value with no space is found.
		Name: "joined no space",
		In:   "cmd=psql;password=hunter2",
		Want: "cmd=psql;password=X",
	}, { // Test 1: The motivating case, space separated, is found.
		Name: "space separated",
		In:   "deploy_cmd=psql password=hunter2",
		Want: "deploy_cmd=psql password=X",
	}, { // Test 2: A non-secret assignment with no nested secret is untouched.
		Name: "plain non-secret",
		In:   "host=web port=8080",
		Want: "host=web port=8080",
	}, { // Test 3: A secret two levels deep with no spaces is still found.
		Name: "double nested",
		In:   "a=b=password=hunter2",
		Want: "a=b=password=X",
	}, { // Test 4: A yaml secret nested behind a colon, whose separator is not '='.
		Name: "yaml nested colon",
		In:   "note: harmless then ansible_ssh_pass: hunter2",
		Want: "note: harmless then ansible_ssh_pass: X",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, _ := RedactAssignments(test.In, "X")
			if got != test.Want {
				t.Errorf("RedactAssignments(%q) = %q, want %q", test.In, got, test.Want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret survived redaction in %q", got)
			}
		})
	}
}

// TestRedactAssignmentsStaysLinear guards the digest hot path against the quadratic scan that a
// crafted body could weaponize. A megabyte run of joined assignments once pinned a core for minutes;
// the redaction must now be linear. The test does not assert a wall-clock time, which would be flaky
// under load: it feeds an input large enough that a quadratic scan cannot finish inside the test
// timeout, so a regression to quadratic fails by timing out while the linear scan returns in
// milliseconds. The spread between the two is minutes versus milliseconds, so there is no middle
// ground for load to push it across.
func TestRedactAssignmentsStaysLinear(t *testing.T) {
	t.Parallel()
	// Both separators can chain, so both are guarded: the equals form the ini pattern reads and the
	// colon form the yaml pattern reads, each the shape that maximized the old rewind's rescanning.
	for _, in := range []string{strings.Repeat("a=", 500000), strings.Repeat("a:", 500000)} {
		got, _ := RedactAssignments(in, "X")
		// No secret keys, so nothing is masked and the text returns unchanged. The point is that it
		// returns at all, in linear time.
		if got != in {
			t.Errorf("a chain of non-secret assignments was altered: len(got)=%d, len(in)=%d",
				len(got), len(in))
		}
	}
}

// TestRedactAssignmentsMasksURLCredentials pins that a credential embedded in a URL, which no
// name=value pattern names, is masked and reported like any other secret.
//
// pg_dump postgres://backup:pass@db and git clone https://x:token@host both carry a password no
// assignment sees. Only the audit digest scrubbed this shape, so a receipt showed it redacted while
// the dossier, the evidence page, and the text sent to an LLM showed it plain, and the run-log
// masker never learned the value. Masking it in the one shared reading closes every path at once.
func TestRedactAssignmentsMasksURLCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       string
		WantMask string
		WantVal  string
	}{{ // Test 0: a postgres URL in a command.
		In:       "pg_dump postgres://backup:Hunter2Pass@db.internal:5432/prod",
		WantMask: "pg_dump postgres://X@db.internal:5432/prod", WantVal: "Hunter2Pass",
	}, { // Test 1: an https URL with a token.
		In:       "git clone https://user:ghp_abc123@github.com/x/y",
		WantMask: "git clone https://X@github.com/x/y", WantVal: "ghp_abc123",
	}, { // Test 2: userinfo with no password contributes nothing to mask.
		In: "ssh://bastion@host", WantMask: "ssh://X@host", WantVal: "",
	}, { // Test 3: an ordinary URL with no userinfo is left alone.
		In: "curl https://example.com/health", WantMask: "curl https://example.com/health", WantVal: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, found := RedactAssignments(test.In, "X")
			if got != test.WantMask {
				t.Errorf("masked = %q, want %q", got, test.WantMask)
			}
			var vals []string
			for _, a := range found {
				vals = append(vals, a.Value)
			}
			if test.WantVal == "" {
				if slices.Contains(vals, test.WantVal) || len(vals) != 0 {
					t.Errorf("found = %v, want no password reported", vals)
				}
				return
			}
			if !slices.Contains(vals, test.WantVal) {
				t.Errorf("found = %v, want the password %q reported so the masker can match it",
					vals, test.WantVal)
			}
		})
	}
}

// TestRedactAssignmentsKeepsAQuotedCompositeIntact covers a secret nested inside a quoted value, the
// most ordinary shape a connection string takes.
//
// The nested scan ran with the quotes still attached, so its unquoted alternative ran to the next
// whitespace and swallowed the closing quote. That broke both outputs at once. The redacted text
// came back missing its quote, and the captured secret came back as `hunter2"` rather than
// `hunter2`, which is the half that mattered: the captured values are what scrub a run's own output,
// so the scrubber searched the log for a string that was never in it, found nothing, and left the
// real secret in the stored log, the events endpoint and the live stream, while the receipt built
// from the same classifier showed it correctly redacted. One install, two answers, and the wrong one
// on the surface a person actually reads.
func TestRedactAssignmentsKeepsAQuotedCompositeIntact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which shape is being redacted.
		Name string
		// In is the raw text.
		In string
		// WantText is the text with the secret masked, quotes intact.
		WantText string
		// WantSecret is the secret exactly as it appears in the run's own output, since that is
		// what the scrubber searches for.
		WantSecret string
	}{{ // Test 0: The ordinary double-quoted connection string.
		Name: "double quoted composite", In: `CONN="host=db user=app password=hunter2"`,
		WantText: `CONN="host=db user=app password=***"`, WantSecret: "hunter2",
	}, { // Test 1: Single quotes behave the same.
		Name: "single quoted composite", In: `CONN='host=db password=hunter2'`,
		WantText: `CONN='host=db password=***'`, WantSecret: "hunter2",
	}, { // Test 2: The secret is the whole quoted value's only assignment.
		Name: "sole nested assignment", In: `CONN="password=abc"`,
		WantText: `CONN="password=***"`, WantSecret: "abc",
	}, { // Test 3: Unquoted is unchanged by this, and still correct.
		Name: "unquoted composite", In: `CONN=host=db;password=hunter2`,
		WantText: `CONN=host=db;password=***`, WantSecret: "hunter2",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			text, found := RedactAssignments(test.In, "***")
			if diff := cmp.Diff(test.WantText, text); diff != "" {
				t.Errorf("%s: redacted text mismatch (-want +got):\n%s", test.Name, diff)
			}
			var got string
			for _, f := range found {
				if f.Name == "password" {
					got = f.Value
				}
			}
			if diff := cmp.Diff(test.WantSecret, got); diff != "" {
				t.Errorf("%s: captured secret mismatch (-want +got):\n%s\nA captured value that is "+
					"not the string in the run's output cannot scrub it.", test.Name, diff)
			}
		})
	}
}
