package ui

import (
	"fmt"
	"regexp"
	"testing"
)

// TestSecretFieldsAreMasked pins that every input holding a secret on the inventory form is a
// password field. The content-source token and key fields were plain text inputs, so a secret typed
// into one showed on screen and in any screen share or recording of the page.
func TestSecretFieldsAreMasked(t *testing.T) {
	t.Parallel()
	page, err := templateFS.ReadFile("templates/inventories.html")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	tests := []struct {
		ID string
	}{{ // Test 0: The Vault token.
		ID: "inv-vault-token",
	}, { // Test 1: The Google access token.
		ID: "inv-gsm-token",
	}, { // Test 2: The AWS secret access key.
		ID: "inv-aws-secret-key",
	}, { // Test 3: The Azure client secret.
		ID: "inv-azure-client-secret",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			input := regexp.MustCompile(`<input id="` + regexp.QuoteMeta(test.ID) + `"[^>]*>`).Find(page)
			if input == nil {
				t.Fatalf("inventories.html has no input %s", test.ID)
			}
			if !regexp.MustCompile(`\btype="password"`).Match(input) {
				t.Errorf("%s is not a password field: %s", test.ID, input)
			}
		})
	}
}

// TestScheduleTimezoneDoesNotSuggestUTC pins the timezone field's placeholder. It read "UTC" while
// an empty field means the server's local time, so a schedule left empty fired hours off.
func TestScheduleTimezoneDoesNotSuggestUTC(t *testing.T) {
	t.Parallel()
	page, err := templateFS.ReadFile("templates/schedules.html")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	input := regexp.MustCompile(`<input id="schedule-timezone"[^>]*>`).Find(page)
	if input == nil {
		t.Fatal("schedules.html has no timezone input")
	}
	if regexp.MustCompile(`placeholder="UTC"`).Match(input) {
		t.Errorf("the timezone placeholder suggests UTC, but empty means server local time: %s", input)
	}
}
