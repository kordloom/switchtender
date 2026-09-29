package safedial

import (
	"strings"
	"testing"
)

// TestBlockedRefusesTheMetadataEndpoint pins the addresses an outbound request must never reach.
//
// 169.254.169.254 is the cloud metadata endpoint. A configurable URL that reaches it returns the
// instance's own credentials to whoever configured it, which turns a notification target or a secret
// source into a credential exfiltration channel. The check runs at the dial, after resolution, so a
// hostname that resolves to it later cannot pass a name check and then connect anyway.
func TestBlockedRefusesTheMetadataEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Address string
		Refused bool
	}{
		{Name: "aws and gcp metadata", Address: "169.254.169.254:80", Refused: true},
		{Name: "azure metadata", Address: "169.254.169.254:443", Refused: true},
		{Name: "link local v6", Address: "[fe80::1]:80", Refused: true},
		{Name: "unspecified v4", Address: "0.0.0.0:80", Refused: true},
		{Name: "unspecified v6", Address: "[::]:80", Refused: true},
		// An internal notification endpoint or secrets server is an ordinary deployment, so private
		// and loopback addresses stay reachable. Refusing them would break real installs to stop an
		// attack that private addresses do not enable.
		{Name: "private network", Address: "10.1.2.3:8080", Refused: false},
		{Name: "loopback", Address: "127.0.0.1:9000", Refused: false},
		{Name: "public", Address: "93.184.216.34:443", Refused: false},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := Blocked(test.Address)
			if refused := err != nil; refused != test.Refused {
				t.Errorf("Blocked(%s) refused = %v, want %v (err %v)",
					test.Address, refused, test.Refused, err)
			}
		})
	}
}

// TestClientDoesNotFollowRedirects checks a redirect cannot turn a checked URL into an unchecked
// one, which is the other half of the same attack.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	c := Client(0)
	if c.CheckRedirect == nil {
		t.Fatal("the client follows redirects, so a target can redirect to an address never checked")
	}
	if err := c.CheckRedirect(nil, nil); err == nil || !strings.Contains(err.Error(), "last response") {
		t.Errorf("CheckRedirect = %v, want the redirect refused", err)
	}
}
