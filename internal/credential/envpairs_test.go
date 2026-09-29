package credential

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEnvPairs pins which env credential material reaches a run and which is refused. A line with no
// key used to be skipped, so a bare value reached the run as nothing and the run went ahead.
func TestEnvPairs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult []string
		Want       error
		In         string
	}{{ // Test 0: Pairs, with the blanks and comments around them dropped.
		In: "# cloud\nAWS_REGION=us-east-1\n\nTOKEN=abc=def\n", WantResult: []string{"AWS_REGION=us-east-1", "TOKEN=abc=def"},
	}, { // Test 1: A bare value is refused.
		In: "s3cr3t", Want: ErrEnvLine,
	}, { // Test 2: A bare line among pairs is refused, not skipped.
		In: "A=1\ns3cr3t\n", Want: ErrEnvLine,
	}, { // Test 3: A line with no key is refused.
		In: "=s3cr3t", Want: ErrEnvLine,
	}, { // Test 4: Nothing at all is nothing, not an error.
		In: "\n# only a comment\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := EnvPairs(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("EnvPairs() error = %v, want %v", err, test.Want)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the refusal carries the secret itself: %v", err)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
