package secretsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
	token := cfg.Token
	if token == "" {
		token = os.Getenv("VAULT_TOKEN")
	}
	if token == "" {
		return "", fmt.Errorf("%w: vault needs a token in the config or VAULT_TOKEN", ErrResolve)
	}

	url := strings.TrimRight(cfg.Addr, "/") + "/v1/" + strings.TrimLeft(cfg.Path, "/")
	if err := checkResolveURL(url); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

// rawToString returns a JSON value as a plain string, unquoting a JSON string and otherwise
// returning the raw JSON, so a non-string field still resolves to something usable.
func rawToString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
