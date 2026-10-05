package util

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

// TestRedactAssignmentsLeavesAnAdjacentQuoteAlone covers a quote that closes one opened before the
// value, which is what a shell command looks like: psql "host=db password=hunter2".
//
// The unquoted alternative runs to the next whitespace, so it captured `hunter2"` and masked the
// quote along with the secret. Both halves were wrong. The text came back missing the quote that
// closed the string, and the captured secret carried one the run's output never had, so the log
// scrubber searched for a string that was not there and left the real password in the log.
func TestRedactAssignmentsLeavesAnAdjacentQuoteAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantText   string
		WantSecret string
	}{{ // Test 0: The shell shape, where the quote opened before the assignment.
		Name: "double quote closing earlier", In: `psql "host=db password=hunter2"`,
		WantText: `psql "host=db password=[redacted]"`, WantSecret: "hunter2",
	}, { // Test 1: Single quotes, with text after, so the quote is plainly not part of the value.
		Name: "single quote closing earlier", In: `echo 'password=abc' && ls`,
		WantText: `echo 'password=[redacted]' && ls`, WantSecret: "abc",
	}, { // Test 2: A bare assignment is untouched by any of this.
		Name: "no quotes at all", In: `password=plain`,
		WantText: `password=[redacted]`, WantSecret: "plain",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			text, found := RedactAssignments(test.In, "[redacted]")
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
				t.Errorf("%s: captured secret mismatch (-want +got):\n%s\nThe captured value is what "+
					"scrubs the run's own output, so one carrying a stray quote scrubs nothing.",
					test.Name, diff)
			}
		})
	}
}

// TestRedactAssignmentsStopsAValueWhereItsSyntaxDoes covers the boundary the patterns cannot see.
//
// Each pattern reads a name and then takes a value, and neither knows what encloses the text: the
// INI form runs to the next whitespace and the YAML form to the end of the line. Both run past the
// real end of a value that a quote opened before the name is holding, or that a shell separator
// closes, and that costs two things at once.
//
// The value handed back is longer than the secret. Callers match it literally against a run's output
// to mask it, so a longer string never matches and a tool that echoed the credential put it in the
// stored log and the live stream while the receipt showed it redacted. And the mask replaced text
// that was not secret, so the command recorded in the signed evidence is not the command that ran:
// case 0 lost the URL it deployed to, case 1 lost the playbook, and case 2 lost the separator that
// made it two commands.
func TestRedactAssignmentsStopsAValueWhereItsSyntaxDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which boundary the case is about.
		Name string
		// In is the text as an operator submits it.
		In string
		// WantText is the redaction, which must keep everything the value does not cover.
		WantText string
		// WantValues are the secrets reported, each exactly as it appears in In so a caller can
		// match it in a run's output.
		WantValues []string
	}{{ // Test 0: A header value held open by a quote that opened before the name.
		Name:       "quoted header",
		In:         `curl -H "X-Api-Key: SUPERSECRET" https://api.example.com/deploy`,
		WantText:   `curl -H "X-Api-Key: ***" https://api.example.com/deploy`,
		WantValues: []string{"SUPERSECRET"},
	}, { // Test 1: The same shape in single quotes, which is how -e carries a YAML scalar.
		Name:       "single quoted scalar",
		In:         `ansible-playbook -e 'vault_password: hunter2' site.yml`,
		WantText:   `ansible-playbook -e 'vault_password: ***' site.yml`,
		WantValues: []string{"hunter2"},
	}, { // Test 2: A separator ends an unquoted value, and the shell would have ended the word there
		// too. Reported with the semicolon, the masker searched the log for abc123; and a tool
		// echoing abc123 left it in the clear.
		Name:       "semicolon ends an unquoted value",
		In:         `sh -c "export API_TOKEN=abc123; deploy"`,
		WantText:   `sh -c "export API_TOKEN=***; deploy"`,
		WantValues: []string{"abc123"},
	}, { // Test 3: A pipe is a separator for the same reason, and written with no space around it
		// the whitespace the INI form stops at is not there to save it.
		Name:       "pipe ends an unquoted value",
		In:         `token=abc123|tee out`,
		WantText:   `token=***|tee out`,
		WantValues: []string{"abc123"},
	}, { // Test 4: A value that opens its own quote is delimited already, and a separator inside it
		// is ordinary text rather than the end of the value. Cutting here would report a fragment,
		// which is the leak in the other direction.
		Name:       "a quote of its own holds a separator",
		In:         `password="a;b" host=db`,
		WantText:   `password=*** host=db`,
		WantValues: []string{"a;b"},
	}, { // Test 5: The value at the end of a quoted composite, where the closing quote is the last
		// character of what the pattern took.
		Name:       "closing quote at the end of the capture",
		In:         `psql "host=db password=hunter2"`,
		WantText:   `psql "host=db password=***"`,
		WantValues: []string{"hunter2"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, found := RedactAssignments(test.In, "***")
			if diff := cmp.Diff(test.WantText, got); diff != "" {
				t.Errorf("redacted text mismatch (-want +got):\n%s", diff)
			}
			values := make([]string, 0, len(found))
			for _, a := range found {
				values = append(values, a.Value)
			}
			if diff := cmp.Diff(test.WantValues, values, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reported values mismatch (-want +got):\n%s", diff)
			}
			for _, v := range test.WantValues {
				if !strings.Contains(test.In, v) {
					t.Errorf("reported %q, which is not in the text it came from, so nothing "+
						"matching it will be found in the run's output either", v)
				}
			}
		})
	}
}

// TestRedactAssignmentsReadsTheWholeShellWord covers values a shell assembles from more than one
// piece: quoted runs joined to each other and to bare text, escapes, and quotes nested inside a
// string a line opened earlier.
//
// The patterns used to take one quoted run or one run of bare text. The shell joins all of them up to
// the first unquoted space, so an empty quote pair before the value masked the quotes and left the
// password in the text, handed the masker an empty string in its place, and redacted differently the
// second time, when the leftover text read as part of the mask's word.
func TestRedactAssignmentsReadsTheWholeShellWord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which shape is being read.
		Name string
		// In is the text as an operator submits it.
		In string
		// WantText is the redaction.
		WantText string
		// WantValues are the values a program receives, which the masker matches in its output.
		WantValues []string
	}{{ // Test 0: An empty quote pair joined onto the value.
		Name: "empty quote pair", In: `password=''x deploy`,
		WantText: `password=X deploy`, WantValues: []string{"x"},
	}, { // Test 1: The idiom for a quote inside single quotes.
		Name: "quote inside single quotes", In: `PGPASSWORD='pa'\''ss' psql`,
		WantText: `PGPASSWORD=X psql`, WantValues: []string{"pa'ss"},
	}, { // Test 2: An escaped space.
		Name: "escaped space", In: `password=x\ y deploy`,
		WantText: `password=X deploy`, WantValues: []string{"x y"},
	}, { // Test 3: Runs of each quote and bare text.
		Name: "mixed runs", In: `password="a"'b'c next`,
		WantText: `password=X next`, WantValues: []string{"abc"},
	}, { // Test 4: ANSI-C quoting.
		Name: "ansi-c", In: `password=$'p\x40ss\tw' next`,
		WantText: `password=X next`, WantValues: []string{"p@ss\tw"},
	}, { // Test 5: An escaped quote inside a string a line opened earlier.
		Name: "escaped quote in a string", In: `sh -c "password=a\"b c"`,
		WantText: `sh -c "password=X c"`, WantValues: []string{`a"b`},
	}, { // Test 6: An escaped quote pair grouping a value inside such a string.
		Name: "escaped quote pair", In: `ssh host "export PASS=\"a b\""`,
		WantText: `ssh host "export PASS=X"`, WantValues: []string{"a b"},
	}, { // Test 7: Single quotes grouping a value inside a double-quoted string.
		Name: "single inside double", In: `sh -c "PASS='a b' deploy"`,
		WantText: `sh -c "PASS=X deploy"`, WantValues: []string{"a b"},
	}, { // Test 8: Double quotes grouping a value inside a single-quoted string.
		Name: "double inside single", In: `echo 'password="a b"'`,
		WantText: `echo 'password=X'`, WantValues: []string{"a b"},
	}, { // Test 9: An apostrophe earlier on the line, which reads as an open quote.
		Name: "apostrophe before", In: `don't reuse password='a b' here`,
		WantText: `don't reuse password=X here`, WantValues: []string{"a b"},
	}, { // Test 10: A quoted value across lines, as a pasted key is.
		Name: "multiline quoted", In: "password=\"line one\nline two\" next",
		WantText: "password=X next", WantValues: []string{"line one\nline two"},
	}, { // Test 11: A backslash joining two lines.
		Name: "line continuation", In: "password=abc\\\ndef next",
		WantText: "password=X next", WantValues: []string{"abcdef"},
	}, { // Test 12: A quoted name, as HCL writes it.
		Name: "quoted name", In: `"db_password" = "hunter2"`,
		WantText: `"db_password" = X`, WantValues: []string{"hunter2"},
	}, { // Test 13: A quote that never closes still takes the rest of the word.
		Name: "unclosed quote", In: `password="abc def`,
		WantText: `password=X def`, WantValues: []string{`"abc`},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, found := RedactAssignments(test.In, "X")
			if diff := cmp.Diff(test.WantText, got); diff != "" {
				t.Errorf("redacted text mismatch (-want +got):\n%s", diff)
			}
			values := make([]string, 0, len(found))
			for _, a := range found {
				values = append(values, a.Value)
			}
			if diff := cmp.Diff(test.WantValues, values, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reported values mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRedactAssignmentsReadsJSONAndYAMLValues covers the name: value form where YAML and JSON write
// their own quoting, which is not the shell's.
//
// A JSON body named its keys in quotes, so the quote between the name and the colon hid every
// secret in it: curl -d '{"password":"x"}' reached the receipt, the dossier, the evidence page, and
// the text sent to an LLM in the clear, and the masker never learned the password.
func TestRedactAssignmentsReadsJSONAndYAMLValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which shape is being read.
		Name string
		// In is the text as an operator submits it.
		In string
		// WantText is the redaction.
		WantText string
		// WantValues are the values a program receives, which the masker matches in its output.
		WantValues []string
	}{{ // Test 0: A compact JSON body on a curl line, which keeps its quotes and stays JSON.
		Name: "json body", In: `curl -d '{"password":"hunter2","user":"bob"}' https://api`,
		WantText:   `curl -d '{"password":"X","user":"bob"}' https://api`,
		WantValues: []string{"hunter2"},
	}, { // Test 1: A spaced JSON document.
		Name: "spaced json", In: `{"api_key": "abc123", "user": "bob"}`,
		WantText: `{"api_key": "X", "user": "bob"}`, WantValues: []string{"abc123"},
	}, { // Test 2: JSON escapes are decoded.
		Name: "json escapes", In: `{"password":"a\"b\\cA"}`,
		WantText: `{"password":"X"}`, WantValues: []string{`a"b\cA`},
	}, { // Test 3: A doubled single quote is one quote.
		Name: "yaml doubled quote", In: `password: 'it''s'`,
		WantText: `password: X`, WantValues: []string{"it's"},
	}, { // Test 4: A comment after a quoted scalar is masked with it.
		Name: "comment after quoted", In: `password: "abc" # note`,
		WantText: `password: X`, WantValues: []string{"abc"},
	}, { // Test 5: A block scalar takes its indented lines.
		Name:       "block scalar",
		In:         "ssh_private_key: |\n  -----BEGIN KEY-----\n  c2VjcmV0\n  -----END KEY-----\nnext: 1",
		WantText:   "ssh_private_key: X\nnext: 1",
		WantValues: []string{"-----BEGIN KEY-----\nc2VjcmV0\n-----END KEY-----"},
	}, { // Test 6: Text joined onto a quoted scalar.
		Name: "joined scalar", In: "token: 'a'b c",
		WantText: "token: X", WantValues: []string{"ab"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, found := RedactAssignments(test.In, "X")
			if diff := cmp.Diff(test.WantText, got); diff != "" {
				t.Errorf("redacted text mismatch (-want +got):\n%s", diff)
			}
			values := make([]string, 0, len(found))
			for _, a := range found {
				values = append(values, a.Value)
			}
			if diff := cmp.Diff(test.WantValues, values, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reported values mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAssignmentReadings pins the strings the masker is handed for one assignment: what a program
// receives first, then the value as written, then Ansible's Python readings of an INI value, of a
// host line after a shell's split and of a group's vars line as written.
func TestAssignmentReadings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the assignment.
		In string
		// WantResult is every reading, in order.
		WantResult []string
	}{
		{In: `ansible_password='"quoted secret"'`, WantResult: []string{`"quoted secret"`, "quoted secret"}}, // Test 0: Ansible's reading.
		{In: `password="a\"b"`, WantResult: []string{`a"b`, `a\"b`}},                                         // Test 1: As written.
		{In: `password=plain`, WantResult: []string{"plain"}},                                                // Test 2: One reading.
		{In: `password=""`, WantResult: nil},                                                                 // Test 3: Nothing.
		{In: `ansible_become_password='multi\nline'`, WantResult: []string{`multi\nline`, "multi\nline"}},    // Test 4: A group's vars line.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, found := RedactAssignments(test.In, "X")
			if len(found) != 1 {
				t.Fatalf("found %d assignments in %q, want 1", len(found), test.In)
			}
			if diff := cmp.Diff(test.WantResult, found[0].Readings(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Readings() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRedactAssignmentsIsIdempotent pins that redacting redacted text changes nothing.
//
// The redacted text is what gets stored, signed, and disclosed, and more than one path redacts the
// same text: a record redacted when it was written is redacted again when a page or a dossier
// serves it. A value cut short left text after the mask that the next pass read as part of it, so
// one redaction and two disagreed, and a digest over one did not match the other. The inputs are
// drawn from the syntax that decides where a value ends, joined at random from a fixed seed.
func TestRedactAssignmentsIsIdempotent(t *testing.T) {
	t.Parallel()
	pieces := []string{"password", "token", "a", "x", "=", ":", " ", "'", `"`, `\`, ";", "\n", "://",
		"@", "#", "|", ",", "}", "$"}
	masks := []string{"«redacted»", "[redacted]", "***", "X"}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(8) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		in := b.String()
		for _, mask := range masks {
			once, _ := RedactAssignments(in, mask)
			if twice, _ := RedactAssignments(once, mask); twice != once {
				t.Fatalf("redacting %q with %q gave %q, and redacting that gave %q", in, mask, once, twice)
			}
		}
	}
}

// FuzzRedactAssignmentsIsIdempotent searches for text whose redaction changes when it is redacted
// again, seeded with shapes that once did.
func FuzzRedactAssignmentsIsIdempotent(f *testing.F) {
	for _, seed := range []string{`password=''x`, `pass:'' b@`, `token="a"b`, `{"password":"x","u":"y"}`,
		"key: |\n  a\nnext: 1", `sh -c "password=a\"b c"`, `password: "abc" # note`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		once, _ := RedactAssignments(in, "«redacted»")
		if twice, _ := RedactAssignments(once, "«redacted»"); twice != once {
			t.Errorf("redacting %q gave %q, and redacting that gave %q", in, once, twice)
		}
	})
}
