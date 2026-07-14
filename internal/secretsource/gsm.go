package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// gsmEndpoint is the Secret Manager API base. It is a var so tests can point it at a mock server.
var gsmEndpoint = "https://secretmanager.googleapis.com"

// gsmMetadataEndpoint is the GCP metadata server base. It is a var so tests can point it at a mock.
var gsmMetadataEndpoint = "http://metadata.google.internal"

// gsmConfig is the JSON a gsm source stores: which secret to read and, optionally, an access token
// to read it with.
type gsmConfig struct {
	// Project is the Google Cloud project id holding the secret.
	Project string `json:"project"`
	// Secret is the secret name.
	Secret string `json:"secret"`
	// Version is the version to read; empty means latest.
	Version string `json:"version,omitempty"`
	// Token is an OAuth2 access token. When empty, one is fetched from the GCP metadata server, so a
	// Railwarden running on GCP reads as its attached service account.
	Token string `json:"token,omitempty"`
}

// resolveGSM reads a secret version from Google Secret Manager over HTTP and returns its value, so a
// source resolves from Secret Manager at run time with no gcloud CLI on the runner. It reads the JSON
// config, authenticates with the config token or a token from the GCP metadata server, and decodes
// the base64 payload. Without a config token it needs Railwarden to run on GCP, since the metadata
// server is the token source.
func resolveGSM(ctx context.Context, config string) (string, error) {
	var cfg gsmConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: gsm config is not valid JSON", ErrResolve)
	}
	if cfg.Project == "" || cfg.Secret == "" {
		return "", fmt.Errorf("%w: gsm config needs project and secret", ErrResolve)
	}
	version := cfg.Version
	if version == "" {
		version = "latest"
	}
	token := cfg.Token
	if token == "" {
		var err error
		token, err = gsmMetadataToken(ctx)
		if err != nil {
			return "", err
		}
	}

	url := fmt.Sprintf("%s/v1/projects/%s/secrets/%s/versions/%s:access",
		strings.TrimRight(gsmEndpoint, "/"), cfg.Project, cfg.Secret, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: gsm request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: gsm returned %s", ErrResolve, resp.Status)
	}

	var out struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: gsm response is not valid JSON", ErrResolve)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return "", fmt.Errorf("%w: gsm payload is not valid base64", ErrResolve)
	}
	return string(decoded), nil
}

// gsmMetadataToken fetches an OAuth2 access token from the GCP metadata server, so a Railwarden on
// GCP reads Secret Manager as its attached service account with no stored credentials.
func gsmMetadataToken(ctx context.Context) (string, error) {
	url := strings.TrimRight(gsmMetadataEndpoint, "/") +
		"/computeMetadata/v1/instance/service-accounts/default/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: gsm needs a token in the config or a GCP metadata server: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: gsm metadata server returned %s", ErrResolve, resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: gsm metadata token missing", ErrResolve)
	}
	return out.AccessToken, nil
}
