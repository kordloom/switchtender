package secretsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kordloom/switchtender/internal/util"
)

// httpMaxBody caps a resolver's HTTP response so a misbehaving endpoint cannot exhaust memory.
const httpMaxBody = 1 << 20

// vaultConfig is the JSON a vault source stores: where to read a secret and, optionally, the token
// to read it with.
type vaultConfig struct {
	// Addr is the Vault address, for example https://vault.example.com:8200.
	Addr string `json:"addr"`
	// Path is the read path, for example secret/data/ci for KV v2 or secret/ci for KV v1.
	Path string `json:"path"`
	// Field is the field within the secret's data to return.
	Field string `json:"field"`
	// Token authenticates the read. When empty, the VAULT_TOKEN environment variable is used.
	Token string `json:"token,omitempty"`
}

// resolveVault reads a single field from a Vault KV secret over HTTP and returns its value, so a
// source resolves from Vault at run time with no vault CLI on the runner. It reads the JSON config,
// authenticates with the config token or VAULT_TOKEN, and extracts the field from a KV v2 response,
// where the secret nests under data.data, or a KV v1 response, where it sits under data.
func resolveVault(ctx context.Context, config string) (string, error) {
	var cfg vaultConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: vault config is not valid JSON", ErrResolve)
	}
	if cfg.Addr == "" || cfg.Path == "" || cfg.Field == "" {
		return "", fmt.Errorf("%w: vault config needs addr, path, and field", ErrResolve)
	}
	token, err := vaultResolveToken(cfg.Token, cfg.Addr)
	if err != nil {
		return "", err
	}

	reqURL := strings.TrimRight(cfg.Addr, "/") + "/v1/" + strings.TrimLeft(cfg.Path, "/")
	if err := checkResolveURL(reqURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: vault request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: vault returned %s", ErrResolve, resp.Status)
	}

	value, err := vaultField(body, cfg.Field)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	return value, nil
}

// vaultField extracts a field from a Vault KV read response, trying the KV v2 shape, where the
// secret nests under data.data, before the KV v1 shape, where it sits directly under data.
func vaultField(body []byte, field string) (string, error) {
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("vault response is not valid JSON")
	}
	if inner, ok := resp.Data["data"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(inner, &nested) == nil {
			if raw, ok := nested[field]; ok {
				return rawToString(raw), nil
			}
		}
	}
	if raw, ok := resp.Data[field]; ok {
		return rawToString(raw), nil
	}
	return "", fmt.Errorf("field %q not found in the vault secret", field)
}

// vaultResolveToken returns the token that authenticates a Vault request. A config token is used
// directly. An empty config token falls back to the VAULT_TOKEN environment secret only when addr
// points at the server's own pinned VAULT_ADDR, so a config-controlled address cannot exfiltrate the
// server's Vault token to an arbitrary host. Any other address must carry its own token in the config.
func vaultResolveToken(configToken, addr string) (string, error) {
	if configToken != "" {
		return configToken, nil
	}
	if envAddr := os.Getenv("VAULT_ADDR"); envAddr != "" && sameVaultEndpoint(addr, envAddr) {
		if token := os.Getenv(util.EnvVaultToken); token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf(
		"%w: vault needs a token in the config, or an addr matching VAULT_ADDR with VAULT_TOKEN set", ErrResolve)
}

// sameVaultEndpoint reports whether two Vault addresses share a scheme and host, so the VAULT_TOKEN
// fallback reaches only the server's own pinned address. Scheme and host compare case-insensitively,
// while a difference in path or trailing slash is ignored, since only the authority decides where the
// token is sent.
func sameVaultEndpoint(a, b string) bool {
	ua, err := url.Parse(strings.TrimRight(a, "/"))
	if err != nil || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(strings.TrimRight(b, "/"))
	if err != nil || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// rawToString returns a JSON value as a plain string, unquoting a JSON string and otherwise
// returning the raw JSON, so a non-string field still resolves to something usable.
func rawToString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
