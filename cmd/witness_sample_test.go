package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	// sampleProducerKey is the key id of the install that signed the published witnessed sample.
	sampleProducerKey = "sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8"
	// sampleWitnessKey is the hosted witness's published key id.
	sampleWitnessKey = "sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804"
)

// sampleBlock returns the fenced block that follows the line ending in after in the witnessed
// sample page, so the page's quoted output is held to what verify prints.
func sampleBlock(t *testing.T, page, after string) string {
	t.Helper()
	i := strings.Index(page, after+"\n\n```\n")
	if i < 0 {
		t.Fatalf("docs/witness-sample.md has no fenced block after %q", after)
	}
	rest := page[i+len(after)+len("\n\n```\n"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatalf("docs/witness-sample.md leaves the block after %q open", after)
	}
	return strings.TrimSpace(rest[:end])
}

// TestTheWitnessedSampleVerifiesAsItsPageSays runs verify over the published witnessed sample and
// holds the output to the page that quotes it: the untouched pair verifies, and a statement edited
// to claim a refusal fails under the witness key.
func TestTheWitnessedSampleVerifiesAsItsPageSays(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "witness-sample.md"))
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	page := string(raw)
	bundle, err := filepath.Abs(filepath.Join("..", "site", "verify", "witness-sample-bundle.json"))
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	statement, err := filepath.Abs(filepath.Join("..", "site", "verify", "witness-sample-statement.json"))
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	edited := editedStatement(t, statement)
	tests := []struct {
		WantOutput string
		Statement  string
		WantCode   bool
	}{{ // Test 0: The published pair verifies and prints what the page quotes.
		Statement: statement, WantCode: true, WantOutput: sampleBlock(t, page, "It prints:"),
	}, { // Test 1: A statement edited to claim a refusal fails as the page says.
		Statement: edited, WantCode: false,
		WantOutput: sampleBlock(t, page, "any edit to it fails under the witness key:"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out, _, code := runCLI(t, "verify", bundle, "--pubkey", sampleProducerKey, "--witness",
				test.Statement, "--witness-pubkey", sampleWitnessKey)
			if got := code == 0; got != test.WantCode {
				t.Errorf("verify exited %d, want success %t:\n%s", code, test.WantCode, out)
			}
			got := strings.TrimSpace(out)
			if !test.WantCode {
				got = got[strings.LastIndex(got, "\n")+1:]
			}
			if diff := cmp.Diff(test.WantOutput, got); diff != "" {
				t.Errorf("verify output differs from the page (-want +got):\n%s", diff)
			}
		})
	}
}

// editedStatement writes a copy of the statement at path with refusals_total raised to one and
// returns its path.
func editedStatement(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the statement: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse the statement: %v", err)
	}
	fields["refusals_total"] = 1
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	edited := filepath.Join(t.TempDir(), "edited.json")
	if err := os.WriteFile(edited, out, 0o600); err != nil {
		t.Fatalf("write the edited statement: %v", err)
	}
	return edited
}
