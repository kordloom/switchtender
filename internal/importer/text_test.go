package importer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/template"
)

// TestApplyRefusesAPlanHoldingUnstorableText pins the check Apply makes before it writes anything:
// the first planned object holding a NUL byte or text that is not valid UTF-8, in any field and at
// any depth, is named with its field, and a plan holding none is let through.
func TestApplyRefusesAPlanHoldingUnstorableText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Plan        *Plan
		Want        error
		WantMessage string
	}{{ // Test 0: A project's name.
		Plan: &Plan{Projects: []*project.Project{{Name: "web\x00"}}}, Want: ErrUnstorableText,
		WantMessage: "the export holds text no store can keep: the project \"web�\" holds a NUL " +
			"byte or text that is not valid UTF-8 in Name",
	}, { // Test 1: An inventory's content, a byte that is not UTF-8.
		Plan: &Plan{Inventories: []*inventory.Inventory{{Name: "prod", Content: "caf\xe9\n"}}},
		Want: ErrUnstorableText,
		WantMessage: "the export holds text no store can keep: the inventory \"prod\" holds a NUL " +
			"byte or text that is not valid UTF-8 in Content",
	}, { // Test 2: A key of a template's extra variables.
		Plan: &Plan{Templates: []*template.Template{{Name: "deploy",
			ExtraVars: map[string]any{"ok": 1, "bad\x00key": "v"}}}},
		Want: ErrUnstorableText,
		WantMessage: "the export holds text no store can keep: the template \"deploy\" holds a NUL " +
			"byte or text that is not valid UTF-8 in ExtraVars key",
	}, { // Test 3: A value nested in a template's extra variables.
		Plan: &Plan{Templates: []*template.Template{{Name: "deploy",
			ExtraVars: map[string]any{"hosts": []any{"a", "b\x00"}}}}},
		Want: ErrUnstorableText,
		WantMessage: "the export holds text no store can keep: the template \"deploy\" holds a NUL " +
			"byte or text that is not valid UTF-8 in ExtraVars[1]",
	}, { // Test 4: A plan whose text is all storable.
		Plan: &Plan{Projects: []*project.Project{{Name: "web"}},
			Inventories: []*inventory.Inventory{{Name: "prod", Content: "café\n"}}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Plan.checkText()
			if !errors.Is(err, test.Want) {
				t.Fatalf("checkText() error = %v, want %v", err, test.Want)
			}
			got := ""
			if err != nil {
				got = err.Error()
			}
			if diff := cmp.Diff(test.WantMessage, got); diff != "" {
				t.Errorf("checkText() message mismatch (-want +got):\n%s", diff)
			}
			if test.Want == nil {
				return
			}
			if _, err := test.Plan.Apply(context.Background(), ApplyStores{}); !errors.Is(err, test.Want) {
				t.Errorf("Apply() error = %v, want %v before any store is touched", err, test.Want)
			}
		})
	}
}
