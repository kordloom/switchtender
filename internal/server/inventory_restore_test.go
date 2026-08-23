package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/inventory"
)

// TestRestoreRedactedInventoryContent proves a local-inventory update never stores the redaction mask
// in place of a real inline secret: an unchanged redacted echo restores the stored secret, plain
// content passes through, and a mask that survives a real edit is refused rather than destroying a
// credential. The trigger in the field is a non-admin manage-grant holder who renames an inventory
// from the redacted view, but the protection is role-agnostic, so it is exercised on the content.
func TestRestoreRedactedInventoryContent(t *testing.T) {
	t.Parallel()
	stored := "[web]\nweb1 ansible_host=10.0.0.5 ansible_password=Hunter2!\n"
	redacted := inventory.Redact(stored)
	if !strings.Contains(redacted, inventory.RedactedValue) {
		t.Fatalf("test setup: Redact did not mask the password: %q", redacted)
	}

	tests := []struct {
		Name        string
		Incoming    string
		WantContent string
		WantRefuse  bool
	}{{ // Test 0: Content with no mask is stored as sent.
		Name: "no mask passes through", Incoming: "[web]\nweb2 ansible_host=10.0.0.9\n",
		WantContent: "[web]\nweb2 ansible_host=10.0.0.9\n",
	}, { // Test 1: A rename that echoes the redacted view keeps the real stored secret.
		Name: "redacted echo restores the stored secret", Incoming: redacted, WantContent: stored,
	}, { // Test 2: A mask on genuinely edited content cannot be tied to a secret, so it is refused.
		Name:       "mask on edited content is refused",
		Incoming:   "[web]\nweb2 ansible_host=10.0.0.6 ansible_password=" + inventory.RedactedValue + "\n",
		WantRefuse: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			gotContent, gotRefuse := restoreRedactedInventoryContent(test.Incoming, stored)
			if test.WantRefuse {
				if gotRefuse == "" {
					t.Fatalf("want refuse, got content %q", gotContent)
				}
				return
			}
			if gotRefuse != "" {
				t.Fatalf("unexpected refuse: %s", gotRefuse)
			}
			if gotContent != test.WantContent {
				t.Errorf("content = %q, want %q", gotContent, test.WantContent)
			}
			if strings.Contains(gotContent, inventory.RedactedValue) {
				t.Errorf("stored content still carries the mask, a secret would be destroyed: %q", gotContent)
			}
		})
	}
}
