package secretsource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ccpConfig is the JSON a ccp source stores: which CyberArk account to read through the Central
// Credential Provider web service and how the calling application authenticates to it. CCP
// authenticates the application by a client certificate or by an allowed-machine rule keyed on the
// caller's address, so there is no bearer token. Locate the account with a safe, folder, and object,
// or with a raw query.
type ccpConfig struct {
	// URL is the CCP base URL, for example https://ccp.example.com.
	URL string `json:"url"`
	// AppID is the CyberArk application id the CCP authorizes.
	AppID string `json:"app_id"`
	// Safe is the safe holding the account. Optional when Query locates it.
	Safe string `json:"safe,omitempty"`
	// Folder is the folder within the safe, usually Root. Optional.
	Folder string `json:"folder,omitempty"`
	// Object is the account object name. Either Object or Query is required.
	Object string `json:"object,omitempty"`
	// Query is a raw CCP query, an alternative to safe, folder, and object.
	Query string `json:"query,omitempty"`
	// Reason is an audit reason CyberArk records for the retrieval. Optional.
	Reason string `json:"reason,omitempty"`
	// WebServiceID is the CCP web service application name in the path. Empty means AIMWebService.
	WebServiceID string `json:"web_service_id,omitempty"`
	// ClientCert is a PEM client certificate for mutual TLS. Pair it with ClientKey. Empty relies on
	// an allowed-machine rule instead.
	ClientCert string `json:"client_cert,omitempty"`
	// ClientKey is the PEM private key for ClientCert.
	ClientKey string `json:"client_key,omitempty"`
	// CACert is a PEM certificate authority that verifies the CCP server, for a private CA. Empty uses
	// the system roots.
	CACert string `json:"ca_cert,omitempty"`
}

// defaultCCPWebService is the standard CyberArk CCP web service application name in the request path.
const defaultCCPWebService = "AIMWebService"

// ccpResponse is the subset of the CCP account JSON that carries the secret. CyberArk returns the
// password in Content alongside account metadata that is ignored here.
type ccpResponse struct {
	// Content is the retrieved secret value.
	Content string `json:"Content"`
}

// resolveCCP reads a secret from CyberArk's Central Credential Provider over its AIMWebService REST
// API and returns the account's Content, so a source resolves from CyberArk at run time with no agent
// or SDK on the runner. It authenticates the application with a client certificate when the config
// carries one, otherwise it relies on a CCP allowed-machine rule keyed on the caller's address.
func resolveCCP(ctx context.Context, config string) (string, error) {
	var cfg ccpConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: ccp config is not valid JSON", ErrResolve)
	}
	if cfg.URL == "" || cfg.AppID == "" {
		return "", fmt.Errorf("%w: ccp config needs url and app_id", ErrResolve)
	}
	if cfg.Object == "" && cfg.Query == "" {
		return "", fmt.Errorf("%w: ccp config needs object or query to locate the account", ErrResolve)
	}

	service := cfg.WebServiceID
	if service == "" {
		service = defaultCCPWebService
	}
	q := url.Values{}
	q.Set("AppID", cfg.AppID)
	for key, val := range map[string]string{
		"Safe": cfg.Safe, "Folder": cfg.Folder, "Object": cfg.Object,
		"Query": cfg.Query, "Reason": cfg.Reason,
	} {
		if val != "" {
			q.Set(key, val)
		}
	}
	reqURL := fmt.Sprintf("%s/%s/api/Accounts?%s",
		strings.TrimRight(cfg.URL, "/"), url.PathEscape(service), q.Encode())
	if err := checkResolveURL(reqURL); err != nil {
		return "", err
	}

	client, err := ccpClient(cfg)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: ccp request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: ccp returned %s", ErrResolve, resp.Status)
	}
	var out ccpResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: ccp response is not valid JSON", ErrResolve)
	}
	if out.Content == "" {
		return "", fmt.Errorf("%w: ccp account has no content", ErrResolve)
	}
	return out.Content, nil
}

// ccpClient builds the HTTP client for a CCP request. With a client certificate it returns a
// mutually-authenticated client that keeps the SSRF dial guard; with only a CA it trusts a private
// CA; with neither it reuses the shared safe client and relies on a CCP allowed-machine rule.
func ccpClient(cfg ccpConfig) (*http.Client, error) {
	var cert *tls.Certificate
	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		pair, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("%w: ccp client certificate is invalid", ErrResolve)
		}
		cert = &pair
	}
	var roots *x509.CertPool
	if cfg.CACert != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(cfg.CACert)) {
			return nil, fmt.Errorf("%w: ccp ca_cert is not a valid certificate", ErrResolve)
		}
	}
	if cert == nil && roots == nil {
		return safeClient, nil
	}
	return safeClientWithTLS(cert, roots), nil
}
