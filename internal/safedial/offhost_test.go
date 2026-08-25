package safedial

import (
	"fmt"
	"testing"
)

// TestACallerSuppliedURLCannotReachThisServer pins that the loopback interface is refused for a URL
// this server was told to fetch by somebody who is not an administrator.
//
// Blocked refuses the unspecified address, and its own comment gives the reason: it reaches the
// local host. 127.0.0.1 reaches the local host more directly and was not refused, so a notification
// target, which rides along with a run and so is named by anyone who may start one, could point the
// server at itself. What listens there is what assumes only local processes reach it.
func TestACallerSuppliedURLCannotReachThisServer(t *testing.T) {
	t.Parallel()
	refused := []string{
		"127.0.0.1:80", "127.0.0.1:8080", "127.99.1.5:80", "[::1]:80",
		"0.0.0.0:80", "[::]:80",
		"169.254.169.254:80", "[fe80::1]:80",
	}
	for testNum, addr := range refused {
		t.Run(fmt.Sprintf("test %d %s", testNum, addr), func(t *testing.T) {
			t.Parallel()
			if err := BlockedOffHost(addr); err == nil {
				t.Errorf("BlockedOffHost(%q) = nil, want a refusal", addr)
			}
		})
	}
}

// TestAnOrdinaryTargetIsStillReachable pins that the refusal did not take the addresses a real
// deployment notifies with it. A self-hosted install pointing at its own internal chat server is
// ordinary, so the private ranges stay reachable.
func TestAnOrdinaryTargetIsStillReachable(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"10.0.0.5:80", "192.168.1.10:443", "172.16.0.9:80", "93.184.216.34:443",
	}
	for testNum, addr := range allowed {
		t.Run(fmt.Sprintf("test %d %s", testNum, addr), func(t *testing.T) {
			t.Parallel()
			if err := BlockedOffHost(addr); err != nil {
				t.Errorf("BlockedOffHost(%q) = %v, want nil", addr, err)
			}
		})
	}
}

// TestTheAdminRuleStillAllowsLoopback pins that the stricter rule was added beside the original
// rather than replacing it. A secret source is configured by an administrator and legitimately
// reads from an agent on the loopback interface, so tightening that would break a real deployment
// to close a hole it does not have.
func TestTheAdminRuleStillAllowsLoopback(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:8200", "[::1]:8200"} {
		if err := Blocked(addr); err != nil {
			t.Errorf("Blocked(%q) = %v, want nil; an admin-configured source may use a local agent",
				addr, err)
		}
	}
	// The addresses the original rule exists for stay refused.
	for _, addr := range []string{"169.254.169.254:80", "0.0.0.0:80"} {
		if err := Blocked(addr); err == nil {
			t.Errorf("Blocked(%q) = nil, want a refusal", addr)
		}
	}
}
