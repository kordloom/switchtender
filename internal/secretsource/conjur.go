package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// conjurConfig is the JSON a conjur source stores: which CyberArk Conjur variable to read and how to
// authenticate. With a token it uses that access token directly. Otherwise it exchanges the API key
// for a short-lived access token at the Conjur authn endpoint, so no long-lived credential is stored
// once a token is issued.
type conjurConfig struct {
	// URL is the Conjur appliance base URL, for example https://conjur.example.com.
	URL string `json:"url"`
	// Account is the Conjur account, sometimes called the organization.
	Account string `json:"account"`
	// Login is the host or user identity the API key belongs to.
	Login string `json:"login,omitempty"`
	// APIKey is the API key exchanged for an access token. Empty requires a token.
	APIKey string `json:"api_key,omitempty"`
	// Variable is the secret variable identifier to read.
	Variable string `json:"variable"`
	// Version reads a specific secret version. Empty reads the current version.
	Version string `json:"version,omitempty"`
	// Token is a base64 access token. When set, it is used directly and no exchange runs.
	Token string `json:"token,omitempty"`
}

// resolveConjur reads a secret variable from CyberArk Conjur over HTTP and returns its value, so a
// source resolves from Conjur at run time with no CyberArk CLI or SDK on the runner. It reads the
// JSON config, obtains an access token from the config or by exchanging the API key, and reads the
// variable.
func resolveConjur(ctx context.Context, config string) (string, error) {
	var cfg conjurConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: conjur config is not valid JSON", ErrResolve)
	}
	if cfg.URL == "" || cfg.Account == "" || cfg.Variable == "" {
		return "", fmt.Errorf("%w: conjur config needs url, account, and variable", ErrResolve)
	}

	base := strings.TrimRight(cfg.URL, "/")
	token, err := conjurToken(ctx, base, cfg)
	if err != nil {
		return "", err
	}

	secretURL := fmt.Sprintf("%s/secrets/%s/variable/%s",
		base, url.PathEscape(cfg.Account), url.PathEscape(cfg.Variable))
	if cfg.Version != "" {
		secretURL += "?version=" + url.QueryEscape(cfg.Version)
	}
	if err := checkResolveURL(secretURL); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, secretURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Authorization", `Token token="`+token+`"`)
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: conjur request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: conjur returned %s", ErrResolve, resp.Status)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("%w: conjur secret has no value", ErrResolve)
	}
	return string(body), nil
}

// conjurToken returns the base64 access token for a Conjur request, using the config token when set
// or exchanging the API key at the authn endpoint. Conjur returns the token as a JSON document that
// the Authorization header carries base64 encoded, so the raw response is encoded here.
func conjurToken(ctx context.Context, base string, cfg conjurConfig) (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}
	if cfg.Login == "" || cfg.APIKey == "" {
		return "", fmt.Errorf("%w: conjur needs login and api_key in the config or a token", ErrResolve)
	}
	authURL := fmt.Sprintf("%s/authn/%s/%s/authenticate",
		base, url.PathEscape(cfg.Account), url.PathEscape(cfg.Login))
	if err := checkResolveURL(authURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(cfg.APIKey))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: conjur authn failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: conjur authn returned %s", ErrResolve, resp.Status)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("%w: conjur authn returned no token", ErrResolve)
	}
	return base64.StdEncoding.EncodeToString(body), nil
}
