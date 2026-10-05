package safedial

import (
	"net/http"
	"testing"
)

// TestTransportsIgnoreTheEnvironmentProxy pins that safedial controls egress at the dial and does not
// send through an ambient HTTP(S)_PROXY, which would route every guarded request to the proxy and
// leave the dial-time metadata and off-host checks seeing only the proxy's address.
//
// The transports once started from http.DefaultTransport.Clone(), which copies Proxy =
// http.ProxyFromEnvironment, so a configured HTTP_PROXY silently turned the SSRF guard off. They now
// consult only an explicitly configured egress proxy and never the environment: with none set, no
// request is proxied whatever the environment says.
func TestTransportsIgnoreTheEnvironmentProxy(t *testing.T) {
	// Not parallel: it sets a process-wide environment variable and the egress proxy.
	t.Setenv("HTTP_PROXY", "http://attacker-proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://attacker-proxy.example:3128")
	t.Setenv("ALL_PROXY", "http://attacker-proxy.example:3128")
	if err := SetEgressProxy(""); err != nil {
		t.Fatalf("SetEgressProxy(\"\") error = %v", err)
	}
	t.Cleanup(func() { _ = SetEgressProxy("") })

	req, _ := http.NewRequest(http.MethodGet, "https://hooks.example.com/x", nil)
	for _, tc := range []struct {
		Name string
		T    *http.Transport
	}{
		{Name: "Transport", T: Transport()},
		{Name: "OffHostTransport", T: OffHostTransport()},
	} {
		if tc.T.Proxy == nil {
			continue // no proxy at all: the environment is not consulted, which is what we want.
		}
		u, err := tc.T.Proxy(req)
		if err != nil {
			t.Errorf("%s proxy function errored on an ordinary request: %v", tc.Name, err)
		}
		if u != nil {
			t.Errorf("%s routes through %s with no egress proxy configured, so it is honoring the "+
				"environment HTTP(S)_PROXY and the dial-time SSRF check never sees the target", tc.Name, u)
		}
	}
}

// TestEgressProxyValidatesTheTargetBeforeSending pins that when an egress proxy is configured, a
// request to the cloud metadata address or to a bare IP literal is refused before anything is sent to
// the proxy. The proxy, not this server, makes the connection, so the target is resolved and held to
// the same blocked-address rules here first.
func TestEgressProxyValidatesTheTargetBeforeSending(t *testing.T) {
	if err := SetEgressProxy("http://egress.example:3128"); err != nil {
		t.Fatalf("SetEgressProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = SetEgressProxy("") })

	tests := []struct {
		// Name is the case.
		Name string
		// URL is the request target.
		URL string
		// WantProxied is whether the request should be sent through the proxy.
		WantProxied bool
	}{ // Test 0: a bare IP literal is refused, since the proxy and not this server resolves it.
		{Name: "ip literal", URL: "http://93.184.216.34/x", WantProxied: false},
		// Test 1: the cloud metadata address, named as an IP, is refused.
		{Name: "metadata ip", URL: "http://169.254.169.254/latest/meta-data/", WantProxied: false},
		// Test 2: a host that resolves to an address the admin rule allows is sent through the proxy.
		// localhost is used so the check resolves locally rather than over the network; Transport uses
		// Blocked, which allows the loopback interface.
		{Name: "ordinary host", URL: "http://localhost:8080/x", WantProxied: true},
	}
	proxyFn := Transport().Proxy
	if proxyFn == nil {
		t.Fatal("Transport().Proxy is nil with an egress proxy configured")
	}
	for testNum, test := range tests {
		req, err := http.NewRequest(http.MethodGet, test.URL, nil)
		if err != nil {
			t.Fatalf("test %d: new request: %v", testNum, err)
		}
		u, err := proxyFn(req)
		proxied := err == nil && u != nil
		if proxied != test.WantProxied {
			t.Errorf("test %d (%s): proxied = %v (proxy %v, err %v), want %v", testNum, test.Name,
				proxied, u, err, test.WantProxied)
		}
	}
}
