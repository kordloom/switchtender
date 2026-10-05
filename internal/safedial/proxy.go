package safedial

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Why safedial does not read the environment proxy.
//
// Every transport here controls egress at the dial: it resolves the target and refuses the cloud
// metadata address, the unspecified address, and, for a caller-supplied URL, this server itself. An
// HTTP(S)_PROXY in the environment defeats that, because the transport then dials the proxy and the
// dial-time check only ever sees the proxy's address, never the target. So these transports never
// inherit the environment proxy: a forgotten HTTP_PROXY cannot silently turn the SSRF guard off.
//
// A deployment that must reach the internet through a proxy sets one explicitly with SetEgressProxy.
// That proxy is used only after the target is validated: a request to an IP literal, or to a hostname
// that resolves to a refused address, is refused before anything is sent to the proxy. A hostname that
// resolves safely now and rebinds to a metadata address after the check is beyond what this can catch,
// because the proxy, not this server, makes the connection, so with an egress proxy the proxy's own
// egress policy is the boundary for DNS rebinding. Point the proxy at an allowlist of hosts the server
// is meant to reach.

// egressProxy holds the explicitly configured egress proxy, nil when none is set.
var egressProxy atomic.Pointer[url.URL]

// proxyResolveTimeout bounds resolving a target host before a proxied request is allowed.
const proxyResolveTimeout = 5 * time.Second

// SetEgressProxy sets the proxy every safedial transport sends through, or clears it when raw is
// empty. The URL must be an absolute http, https, or socks5 URL. It is set once at startup from the
// server's configuration, never from the ambient environment.
func SetEgressProxy(raw string) error {
	if raw == "" {
		egressProxy.Store(nil)
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("egress proxy %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("egress proxy %q must be an http, https, or socks5 URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("egress proxy %q names no host", raw)
	}
	egressProxy.Store(u)
	return nil
}

// EgressProxy returns the configured egress proxy, or nil when none is set.
func EgressProxy() *url.URL {
	return egressProxy.Load()
}

// proxyFor returns the Proxy function a transport uses: it sends through the configured egress proxy,
// but only after validating the request's target with blocked, and returns no proxy at all when none
// is configured, so the transport dials directly and the dial-time check applies. blocked is the same
// rule the transport's own dial control uses, [Blocked] or [BlockedOffHost].
func proxyFor(blocked func(string) error) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		proxy := egressProxy.Load()
		if proxy == nil {
			return nil, nil
		}
		if err := validateProxyTarget(req.Context(), req.URL, blocked); err != nil {
			return nil, err
		}
		return proxy, nil
	}
}

// validateProxyTarget refuses a target a proxied request must not carry: one named by an IP literal,
// one with no host, and one whose host resolves to an address blocked reports. The check is made here
// because a proxied request never reaches the dial-time control, which sees only the proxy.
func validateProxyTarget(ctx context.Context, u *url.URL, blocked func(string) error) error {
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("a proxied request names no host")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("a proxied request to the IP literal %q is refused, since the proxy and not "+
			"this server resolves it, so name a host the proxy's egress policy allows", host)
	}
	port := u.Port()
	if port == "" {
		port = "0"
	}
	ctx, cancel := context.WithTimeout(ctx, proxyResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("the target host %q could not be resolved to check it: %w", host, err)
	}
	for _, a := range addrs {
		if err := blocked(net.JoinHostPort(a.IP.String(), port)); err != nil {
			return fmt.Errorf("the target host %q resolves to %s, which is refused: %w", host, a.IP, err)
		}
	}
	return nil
}
