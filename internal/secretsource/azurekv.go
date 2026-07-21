package secretsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// azureAPIVersion is the Key Vault data-plane API version a resolve calls.
const azureAPIVersion = "7.4"

// azureResource is the Key Vault resource a token is scoped to, used as the client-credentials scope
// and the managed-identity resource parameter.
const azureResource = "https://vault.azure.net"

// azureEndpoint overrides the computed vault base URL. It is empty in production, where the base is
// derived from the vault name, and set by tests to point at a mock server.
var azureEndpoint = ""

// azureAuthEndpoint is the Entra ID token base for the client-credentials grant. It is a var so tests
// can point it at a mock server.
var azureAuthEndpoint = "https://login.microsoftonline.com"

// azureIMDSEndpoint is the Azure Instance Metadata Service base a managed-identity token comes from.
// It is a var so tests can point it at a mock server.
var azureIMDSEndpoint = "http://169.254.169.254"

// azureConfig is the JSON an azure source stores: which secret to read and how to authenticate. With
// a token it uses that bearer directly. With a tenant, client id, and client secret it runs the
// client-credentials grant. With none of those it reads a managed-identity token from the Azure
// Instance Metadata Service, so a SwitchTender running on Azure reads as its attached identity with no
// stored secret.
type azureConfig struct {
	// Vault is the Key Vault name, the name in name.vault.azure.net.
	Vault string `json:"vault"`
	// Secret is the secret name.
	Secret string `json:"secret"`
	// Version reads a specific secret version. Empty reads the current version.
	Version string `json:"version,omitempty"`
	// TenantID is the Entra ID tenant for the client-credentials grant.
	TenantID string `json:"tenant_id,omitempty"`
	// ClientID is the service principal application id.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is the service principal secret.
	ClientSecret string `json:"client_secret,omitempty"`
	// Token is a bearer token for Key Vault. When set, it is used directly and no grant runs.
	Token string `json:"token,omitempty"`
}

// resolveAzure reads a secret from Azure Key Vault over HTTP and returns its value, so a source
// resolves from Key Vault at run time with no az CLI or Azure SDK on the runner. It reads the JSON
// config, obtains a bearer token from the config, a client-credentials grant, or the Azure Instance
// Metadata Service, and returns the secret value.
func resolveAzure(ctx context.Context, config string) (string, error) {
	var cfg azureConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: azure config is not valid JSON", ErrResolve)
	}
	if cfg.Vault == "" || cfg.Secret == "" {
		return "", fmt.Errorf("%w: azure config needs vault and secret", ErrResolve)
	}

	base := azureEndpoint
	if base == "" {
		base = fmt.Sprintf("https://%s.vault.azure.net", cfg.Vault)
	}
	base = strings.TrimRight(base, "/")
	secretURL := fmt.Sprintf("%s/secrets/%s?api-version=%s", base, cfg.Secret, azureAPIVersion)
	if cfg.Version != "" {
		secretURL = fmt.Sprintf("%s/secrets/%s/%s?api-version=%s", base, cfg.Secret, cfg.Version, azureAPIVersion)
	}
	if err := checkResolveURL(secretURL); err != nil {
		return "", err
	}

	token, err := azureToken(ctx, cfg)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, secretURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: azure request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: azure returned %s", ErrResolve, resp.Status)
	}

	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: azure response is not valid JSON", ErrResolve)
	}
	if out.Value == "" {
		return "", fmt.Errorf("%w: azure secret has no value", ErrResolve)
	}
	return out.Value, nil
}

// azureToken obtains a Key Vault bearer token from the config: a config token used directly, a
// client-credentials grant when a tenant, client id, and client secret are set, or a managed-identity
// token from the Azure Instance Metadata Service when none are. A partial service principal is an
// error, since it is a misconfiguration rather than a request for managed identity.
func azureToken(ctx context.Context, cfg azureConfig) (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}
	if cfg.TenantID != "" || cfg.ClientID != "" || cfg.ClientSecret != "" {
		if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
			return "", fmt.Errorf(
				"%w: azure service principal needs tenant_id, client_id, and client_secret", ErrResolve)
		}
		return azureClientCredentialsToken(ctx, cfg)
	}
	return azureIMDSToken(ctx)
}

// azureClientCredentialsToken runs the OAuth2 client-credentials grant against Entra ID and returns
// the access token, so a service principal reads Key Vault with no Azure SDK on the runner.
func azureClientCredentialsToken(ctx context.Context, cfg azureConfig) (string, error) {
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(azureAuthEndpoint, "/"), cfg.TenantID)
	if err := checkResolveURL(tokenURL); err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"scope":         {azureResource + "/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: azure token request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: azure token endpoint returned %s", ErrResolve, resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: azure token response missing access_token", ErrResolve)
	}
	return out.AccessToken, nil
}

// azureIMDSToken reads a managed-identity access token for Key Vault from the Azure Instance Metadata
// Service, so a SwitchTender on Azure reads as its attached identity with no stored secret. It uses
// metadataClient, which may reach the link-local metadata address, and is called only with the fixed
// IMDS endpoint, never a config-derived URL.
func azureIMDSToken(ctx context.Context) (string, error) {
	imdsURL := fmt.Sprintf("%s/metadata/identity/oauth2/token?api-version=2018-02-01&resource=%s",
		strings.TrimRight(azureIMDSEndpoint, "/"), url.QueryEscape(azureResource))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Metadata", "true")
	resp, err := metadataClient.Do(req)
	if err != nil {
		return "", fmt.Errorf(
			"%w: azure needs a token, a service principal, or an Azure metadata service: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: azure metadata service returned %s", ErrResolve, resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: azure metadata token missing", ErrResolve)
	}
	return out.AccessToken, nil
}
