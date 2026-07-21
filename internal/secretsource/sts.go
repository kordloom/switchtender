package secretsource

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// awsSTSService is the SigV4 service name for the Security Token Service.
const awsSTSService = "sts"

// awsSTSAPIVersion is the STS query API version for AssumeRole.
const awsSTSAPIVersion = "2011-06-15"

// defaultAWSSTSDuration is the assumed-role credential lifetime when the config sets none, one hour,
// the STS default.
const defaultAWSSTSDuration = 3600

// defaultAWSSTSSessionName is the role session name when the config sets none.
const defaultAWSSTSSessionName = "switchtender"

// awsSTSEndpoint overrides the computed regional endpoint. It is empty in production, where the
// endpoint is derived from the region, and set by tests to point at a mock server.
var awsSTSEndpoint = ""

// awsSTSConfig is the JSON an aws_sts source stores: which role to assume and the base credentials to
// sign the AssumeRole call with. Empty credential fields fall back to the standard AWS environment, so
// a SwitchTender running with an injected key assumes the target role with no stored key. The
// assumed-role credentials are minted fresh for each run and expire on their own STS lifetime.
type awsSTSConfig struct {
	// RoleARN is the ARN of the role to assume.
	RoleARN string `json:"role_arn"`
	// RoleSessionName names the session in CloudTrail. Empty means switchtender.
	RoleSessionName string `json:"role_session_name,omitempty"`
	// Region is the STS region for the endpoint and signature. Empty falls back to AWS_REGION, then
	// AWS_DEFAULT_REGION.
	Region string `json:"region,omitempty"`
	// DurationSeconds is the assumed-role credential lifetime. Empty means one hour.
	DurationSeconds int `json:"duration_seconds,omitempty"`
	// ExternalID is the external id a cross-account role trust may require. Optional.
	ExternalID string `json:"external_id,omitempty"`
	// AccessKeyID is the base access key that signs the call. Empty falls back to AWS_ACCESS_KEY_ID.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// SecretAccessKey is the base secret key. Empty falls back to AWS_SECRET_ACCESS_KEY.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// SessionToken is an optional base session token. Empty falls back to AWS_SESSION_TOKEN.
	SessionToken string `json:"session_token,omitempty"`
}

// stsAssumeRoleResponse is the subset of the AssumeRole XML that carries the minted credentials.
type stsAssumeRoleResponse struct {
	// Credentials holds the short-lived key, secret, and session token STS issued.
	Credentials struct {
		// AccessKeyID is the temporary access key.
		AccessKeyID string `xml:"AccessKeyId"`
		// SecretAccessKey is the temporary secret key.
		SecretAccessKey string `xml:"SecretAccessKey"`
		// SessionToken is the temporary session token.
		SessionToken string `xml:"SessionToken"`
	} `xml:"AssumeRoleResult>Credentials"`
}

// mintAWSSTS assumes an IAM role through AWS STS and returns short-lived credentials as an env block
// for the env credential kind, so a run reaches AWS as the assumed role with no long-lived key on the
// runner. It signs the AssumeRole call with the base credentials using Signature Version 4, sharing
// the signer with Secrets Manager. The returned lease is a no-op, since STS credentials cannot be
// revoked early and expire on their own lifetime.
func mintAWSSTS(ctx context.Context, config string) (string, *Lease, error) {
	var cfg awsSTSConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", nil, fmt.Errorf("%w: aws_sts config is not valid JSON", ErrResolve)
	}
	if cfg.RoleARN == "" {
		return "", nil, fmt.Errorf("%w: aws_sts config needs role_arn", ErrResolve)
	}
	creds, region, err := awsResolveCredentials(awsConfig{
		Region: cfg.Region, AccessKeyID: cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey, SessionToken: cfg.SessionToken,
	})
	if err != nil {
		return "", nil, err
	}

	endpoint := awsSTSEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://sts.%s.amazonaws.com/", region)
	}
	if err := checkResolveURL(endpoint); err != nil {
		return "", nil, err
	}

	sessionName := cfg.RoleSessionName
	if sessionName == "" {
		sessionName = defaultAWSSTSSessionName
	}
	duration := cfg.DurationSeconds
	if duration <= 0 {
		duration = defaultAWSSTSDuration
	}
	form := url.Values{}
	form.Set("Action", "AssumeRole")
	form.Set("Version", awsSTSAPIVersion)
	form.Set("RoleArn", cfg.RoleARN)
	form.Set("RoleSessionName", sessionName)
	form.Set("DurationSeconds", strconv.Itoa(duration))
	if cfg.ExternalID != "" {
		form.Set("ExternalId", cfg.ExternalID)
	}
	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signAWSV4(req, body, creds, region, awsSTSService, time.Now())

	resp, err := safeClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: aws_sts request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%w: aws_sts returned %s", ErrResolve, resp.Status)
	}
	var out stsAssumeRoleResponse
	if err := xml.Unmarshal(respBody, &out); err != nil {
		return "", nil, fmt.Errorf("%w: aws_sts response is not valid XML", ErrResolve)
	}
	c := out.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return "", nil, fmt.Errorf("%w: aws_sts returned no credentials", ErrResolve)
	}
	env := strings.Join([]string{
		"AWS_ACCESS_KEY_ID=" + c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + c.SessionToken,
	}, "\n")
	return env, NewLease(KindAWSSTS, nil), nil
}
