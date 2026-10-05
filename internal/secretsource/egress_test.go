package secretsource

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/kordloom/switchtender/internal/safedial"
)

// TestSecretSourcesIgnoreTheEnvironmentProxy pins that a secret source's request is never sent to an
// ambient HTTP(S)_PROXY. The source transport once started from the default transport, which sends
// every request to that proxy, so the dial check only ever saw the proxy's address and a source whose
// name resolves to the cloud metadata address was fetched by the proxy. The workload-identity token
// fetch is never proxied at all.
func TestSecretSourcesIgnoreTheEnvironmentProxy(t *testing.T) {
	// Not parallel: it sets process-wide environment variables and the egress proxy.
	t.Setenv("HTTP_PROXY", "http://attacker-proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://attacker-proxy.example:3128")
	t.Setenv("ALL_PROXY", "http://attacker-proxy.example:3128")
	if err := safedial.SetEgressProxy(""); err != nil {
		t.Fatalf("SetEgressProxy(\"\") error = %v", err)
	}
	t.Cleanup(func() { _ = safedial.SetEgressProxy("") })

	req, err := http.NewRequest(http.MethodGet, "https://vault.example.com/v1/secret/data/db", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	for name, client := range map[string]*http.Client{
		"source": safeClient, "source with TLS": safeClientWithTLS(nil, nil),
		"metadata": metadataClient,
	} {
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s client transport is %T, want *http.Transport", name, client.Transport)
		}
		if tr.Proxy == nil {
			continue
		}
		if u, err := tr.Proxy(req); err != nil || u != nil {
			t.Errorf("%s client routes through %v (%v) with no egress proxy configured, so it is "+
				"honoring the environment proxy and the dial check never sees the target", name, u, err)
		}
	}
}

// TestSecretSourcesUseTheEgressProxyAfterTheCheck pins that a configured egress proxy carries a
// source's request only after its target passes the same address check the dial makes, and that the
// workload-identity token fetch goes to its fixed endpoint directly rather than through the proxy.
func TestSecretSourcesUseTheEgressProxyAfterTheCheck(t *testing.T) {
	// Not parallel: it sets the process-wide egress proxy.
	const egress = "http://egress.example:3128"
	if err := safedial.SetEgressProxy(egress); err != nil {
		t.Fatalf("SetEgressProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = safedial.SetEgressProxy("") })

	tests := []struct {
		// URL is the request target.
		URL string
		// WantProxied is whether the source's request goes through the egress proxy.
		WantProxied bool
	}{{ // Test 0: A source on a host that resolves to an allowed address goes through the proxy.
		URL: "http://localhost:8200/v1/secret/data/db", WantProxied: true,
	}, { // Test 1: The cloud metadata address is refused before anything reaches the proxy.
		URL: "http://169.254.169.254/latest/meta-data/", WantProxied: false,
	}}
	proxyFn := safeTransport().Proxy
	if proxyFn == nil {
		t.Fatal("the source transport consults no proxy with an egress proxy configured")
	}
	want, _ := url.Parse(egress)
	for testNum, test := range tests {
		req, err := http.NewRequest(http.MethodGet, test.URL, nil)
		if err != nil {
			t.Fatalf("test %d: NewRequest() error = %v", testNum, err)
		}
		u, err := proxyFn(req)
		proxied := err == nil && u != nil && u.String() == want.String()
		if proxied != test.WantProxied {
			t.Errorf("test %d: proxied = %v (proxy %v, err %v), want %v", testNum, proxied, u, err,
				test.WantProxied)
		}
	}
	if tr, ok := metadataClient.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("the workload-identity token fetch can be sent through a proxy, which cannot reach " +
			"this host's metadata endpoint and could answer with a token of its choosing")
	}
}
