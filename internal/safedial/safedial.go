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
//
// Loopback and the private ranges are deliberately allowed here. This rule guards the URLs an
// administrator configures, a secret source or a project remote, and those legitimately reach a
// secrets agent on the loopback interface or a git host inside the network. A caller-supplied URL
// is a different matter and gets [BlockedOffHost] instead.
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
	// Not every cloud puts its metadata service on a link-local address. Alibaba answers on
	// 100.100.100.200 and AWS serves an IPv6 endpoint at fd00:ec2::254, neither of which is
	// link-local, so both were reachable on those platforms while the package documented blocking
	// the metadata endpoint. They are named individually rather than by their surrounding ranges
	// because 100.64.0.0/10 is ordinary carrier space and fc00::/7 is the IPv6 equivalent of the
	// private ranges this function deliberately allows.
	for _, meta := range metadataIPs {
		if ip.Equal(meta) {
			return fmt.Errorf("resolved address %q is a cloud metadata endpoint", host)
		}
	}
	return nil
}

// metadataIPs are the cloud metadata endpoints that sit outside link-local space, which the
// link-local test above does not reach.
var metadataIPs = []net.IP{
	net.ParseIP("100.100.100.200"),
	net.ParseIP("fd00:ec2::254"),
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

// BlockedOffHost is [Blocked] with the loopback interface refused as well, for a URL this server
// was told to fetch by somebody who is not an administrator.
//
// A notification target rides along with a run, so anyone who may start a run may name an address
// the server then connects to from its own network position. Blocked alone let that be the machine
// the server is running on, which is where the services that assume only local processes reach them
// listen: an admin socket, an unauthenticated exporter, the server's own API. The comment on Blocked
// already said the unspecified address is refused for reaching the local host; 127.0.0.1 reaches it
// far more directly and was not refused.
//
// Checking the URL text instead would not close it. An attacker does not need a rebinding trick,
// only a DNS record for a name they own pointing at 127.0.0.1, so the refusal has to happen once the
// address is resolved, which is here.
//
// The private ranges stay reachable. A self-hosted install notifying its own internal chat server is
// ordinary, and refusing that would break more than it protects.
func BlockedOffHost(address string) error {
	if err := Blocked(address); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("split resolved address: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return fmt.Errorf("resolved address %q is this server itself", host)
	}
	return nil
}

// ControlOffHost is the dialer hook that applies [BlockedOffHost].
func ControlOffHost(_, address string, _ syscall.RawConn) error {
	return BlockedOffHost(address)
}

// OffHostClient returns an HTTP client for a caller-supplied URL: it refuses an unsafe address,
// refuses this server itself, and does not follow redirects.
func OffHostClient(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialTimeout,
		Control:   ControlOffHost,
	}).DialContext
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     t,
	}
}
