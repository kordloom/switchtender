package policy

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRegoInputNamesTheAccountId holds the actor's account under account_id, the name the schema and
// the policy docs give it, and drops the older bare name.
func TestRegoInputNamesTheAccountId(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		ActorUser string
		WantID    any
	}{{ // Test 0: A run from a named account carries its id.
		Name:      "named account",
		ActorUser: "usr_42",
		WantID:    "usr_42",
	}, { // Test 1: A run with no account carries an empty id.
		Name:   "no account",
		WantID: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ActorUserID: test.ActorUser}
			actor, ok := RegoInput(r)["actor"].(map[string]any)
			if !ok {
				t.Fatal("RegoInput has no actor map")
			}
			if diff := cmp.Diff(test.WantID, actor["account_id"]); diff != "" {
				t.Errorf("account_id mismatch (-want +got):\n%s", diff)
			}
			if _, has := actor["account"]; has {
				t.Error("the bare account key is still present")
			}
		})
	}
}
