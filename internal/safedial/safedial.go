// Package safedial refuses outbound connections to addresses a server-side request forgery aims at.
//
// A URL an operator can configure, a webhook target, a notification channel, a secret source, a
// project remote, is an instruction the server follows on its own network. Left unguarded it reaches
// the cloud metadata endpoint and returns instance credentials to whoever set it. The name check
// alone cannot stop that, because a hostname the attacker controls can resolve to the metadata
// address after the check passes, so the refusal has to happen at the dial, once the address is
// known.
//
// It lives here rather than beside any one caller because every outbound path needs the same rule,
// and a rule that exists in several copies is a rule that will drift.
package safedial

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// dialTimeout bounds a connection attempt to an address that may not answer.
const dialTimeout = 30 * time.Second

// Blocked reports why a resolved address must not be dialed, or nil when it may be.
//
// Link-local covers the cloud metadata endpoint at 169.254.169.254, which is the address these
// attacks want. The unspecified address is refused because it reaches the local host.
func Blocked(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("split resolved address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("could not parse resolved address %q", host)
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("resolved address %q is not allowed", host)
	}
	return nil
}

// Control is the dialer hook that applies Blocked, suitable for net.Dialer.Control.
func Control(_, address string, _ syscall.RawConn) error {
	return Blocked(address)
}

// Transport returns a transport that refuses an unsafe address at the dial.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialTimeout,
		Control:   Control,
	}).DialContext
	return t
}

// Client returns an HTTP client that refuses an unsafe address and does not follow redirects, since
// a redirect is the other way a checked URL becomes an unchecked one.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     Transport(),
	}
}
