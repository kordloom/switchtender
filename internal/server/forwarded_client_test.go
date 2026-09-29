package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestForwardedClientReadsFromTheRightPastTrustedHops pins the rate-limiter key against the
// client's own forging.
//
// X-Forwarded-For grows on the left: each proxy appends, so the leftmost entry is whatever the
// original client wrote and can freely change. Keying the limiter on it let a caller send a fresh
// leftmost address every request and mint an unbounded set of keys, evading the per-client brake
// entirely. The real client is the last address not written by a proxy this install trusts, found
// by walking from the right past the trusted hops.
func TestForwardedClientReadsFromTheRightPastTrustedHops(t *testing.T) {
	t.Parallel()
	_, proxyNet, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	// The proxy list is passed in rather than installed in the package globals: those are boot-set
	// configuration shared by every request, and a test that mutates them races every parallel
	// test in the package, which is its own copy of the defect this file guards against.
	proxies := []*net.IPNet{proxyNet}

	get := func(xff string) string {
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		r.Header.Set("X-Forwarded-For", xff)
		return forwardedClientIn(r, "", proxies)
	}

	tests := []struct {
		// Name says what the chain represents.
		Name string
		// XFF is the header value as it would arrive.
		XFF string
		// Want is the address the limiter must key on.
		Want string
	}{
		{"one real client through one proxy", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{"the client forged a leftmost address", "1.1.1.1, 203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{"two trusted hops", "203.0.113.9, 10.0.0.2, 10.0.0.1", "203.0.113.9"},
		{"a forged trusted-looking prefix is still bypassed",
			"10.9.9.9, 203.0.113.9, 10.0.0.1", "203.0.113.9"},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if got := get(test.XFF); got != test.Want {
				t.Errorf("forwardedClient(%q) = %q, want %q: the limiter keys on the wrong "+
					"address and a client can forge its way around the brake", test.XFF, got, test.Want)
			}
		})
	}

	// A malformed entry breaks the chain: everything left of it is unverifiable, so the function
	// declines to guess rather than trust a value it cannot place.
	if got := get("not-an-ip, 10.0.0.1"); got != "" {
		t.Errorf("a broken chain returned %q, want empty", got)
	}
}
