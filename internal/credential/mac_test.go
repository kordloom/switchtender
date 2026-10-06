package credential

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// TestMACBindsKeyAndPurpose proves a MAC depends on the sealer's key and on the purpose, and that a
// sealer with no key refuses to make one.
func TestMACBindsKeyAndPurpose(t *testing.T) {
	t.Parallel()
	server := NewSealer("server-passphrase", "server-salt")
	other := NewSealer("other-passphrase", "server-salt")
	base, err := server.MAC("forge-link-state", []byte("payload"))
	if err != nil {
		t.Fatalf("MAC() error = %v", err)
	}
	tests := []struct {
		Sealer   *Sealer
		Purpose  string
		Data     string
		WantSame bool
		Want     error
	}{{ // Test 0: The same key, purpose, and data give the same MAC.
		Sealer: server, Purpose: "forge-link-state", Data: "payload", WantSame: true,
	}, { // Test 1: Another server's key gives another MAC.
		Sealer: other, Purpose: "forge-link-state", Data: "payload",
	}, { // Test 2: Another purpose gives another MAC.
		Sealer: server, Purpose: "another-purpose", Data: "payload",
	}, { // Test 3: Other data gives another MAC.
		Sealer: server, Purpose: "forge-link-state", Data: "payload2",
	}, { // Test 4: A sealer with no key makes no MAC.
		Sealer: NewSealer("", ""), Purpose: "forge-link-state", Data: "payload", Want: ErrNoKey,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := test.Sealer.MAC(test.Purpose, []byte(test.Data))
			if !errors.Is(err, test.Want) {
				t.Fatalf("MAC() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if same := bytes.Equal(got, base); same != test.WantSame {
				t.Errorf("MAC() same as the base = %v, want %v", same, test.WantSame)
			}
		})
	}
}
