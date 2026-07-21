package secretsource

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// blockedResolveHosts are hostnames a secret source must never reach. The cloud metadata service can
// hand back instance credentials, so a request to it is a server-side request forgery.
var blockedResolveHosts = map[string]bool{
	"metadata.google.internal": true,
	"metadata":                 true,
}

// safeClient is the HTTP client the resolvers use. It refuses to follow redirects, so an auth token is
// never re-sent to a host a redirect chose, and it bounds every request with a timeout.
var safeClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     safeTransport(),
}

// metadataClient fetches a workload-identity token from a cloud metadata service, such as the GCP
// metadata server or the Azure Instance Metadata Service. Those endpoints live on a link-local
// address that safeClient refuses, since for a config-controlled resolve that address is a
// server-side request forgery target that can hand back instance credentials. A workload-identity
// fetch is the opposite: the endpoint is a fixed address SwitchTender hardcodes, never one config
// chooses, so reaching it is the intent. Use this client only with those hardcoded endpoints.
var metadataClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// safeTransport clones the default transport and installs a dialer that rejects a connection whose
// resolved address is link-local or unspecified, so a hostname that resolves to the cloud metadata
// service cannot slip past the name check.
func safeTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockUnsafeDial,
	}).DialContext
	return t
}

// blockUnsafeDial rejects a resolved address that is link-local, link-local multicast, or
// unspecified, which covers the cloud metadata endpoint, so a rebinding hostname cannot reach it.
func blockUnsafeDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrResolve, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: could not parse resolved address %q", ErrResolve, host)
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: resolved address %q is not allowed", ErrResolve, host)
	}
	return nil
}

// checkResolveURL rejects a secret source address that is not http or https, has no host, or points at
// a cloud metadata or link-local endpoint, so a config-controlled source cannot drive the executor
// into a server-side request forgery that steals instance credentials. Loopback and private hosts pass,
// since an internal secrets server is a normal deployment.
func checkResolveURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrResolve, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: address must be http or https", ErrResolve)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: address has no host", ErrResolve)
	}
	if blockedResolveHosts[strings.ToLower(host)] {
		return fmt.Errorf("%w: host %q is not allowed", ErrResolve, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: address %q is not allowed", ErrResolve, host)
		}
	}
	return nil
}
