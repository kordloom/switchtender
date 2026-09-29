package cmd

import (
	"fmt"
	"testing"
)

// TestCreatedLineSaysOnlyWhatTheImportNeeds pins the import's closing line. It told every import to
// re-enter credential secrets, including a crontab or a Chef fleet that creates no credentials, and
// said "1 objects".
func TestCreatedLineSaysOnlyWhatTheImportNeeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Created, NeedSecret int
		WantLine            string
	}{{ // Test 0: No credentials, no instruction about them.
		Created: 12, WantLine: "Created 12 objects.",
	}, { // Test 1: One object is singular.
		Created: 1, WantLine: "Created 1 object.",
	}, { // Test 2: Credentials without secrets are counted.
		Created: 9, NeedSecret: 3,
		WantLine: "Created 9 objects. 3 credentials have no secret yet: enter it before running a template that needs it.",
	}, { // Test 3: One credential is singular.
		Created: 4, NeedSecret: 1,
		WantLine: "Created 4 objects. 1 credential has no secret yet: enter it before running a template that needs it.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := createdLine(test.Created, test.NeedSecret); got != test.WantLine {
				t.Errorf("createdLine(%d, %d) = %q, want %q", test.Created, test.NeedSecret, got, test.WantLine)
			}
		})
	}
}
