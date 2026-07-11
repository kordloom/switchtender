package secretsource

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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
