package secretsource

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// awsService is the SigV4 service name for Secrets Manager.
const awsService = "secretsmanager"

// awsTarget is the Secrets Manager JSON API action a resolve calls.
const awsTarget = "secretsmanager.GetSecretValue"

// awsContentType is the content type Secrets Manager's JSON protocol requires.
const awsContentType = "application/x-amz-json-1.1"

// awsEndpoint overrides the computed regional endpoint. It is empty in production, where the
// endpoint is derived from the region, and set by tests to point at a mock server.
var awsEndpoint = ""

// awsConfig is the JSON an aws source stores: which secret to read and the credentials to read it
// with. Empty credential fields fall back to the standard AWS environment variables, so a
// SwitchTender running with an instance role or an injected key reads with no stored secret.
type awsConfig struct {
	// SecretID is the secret's name or full ARN.
	SecretID string `json:"secret_id"`
	// Region is the AWS region holding the secret, for example us-east-1.
	Region string `json:"region,omitempty"`
	// AccessKeyID is the access key. Empty falls back to AWS_ACCESS_KEY_ID.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// SecretAccessKey is the secret key. Empty falls back to AWS_SECRET_ACCESS_KEY.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// SessionToken is an optional STS session token. Empty falls back to AWS_SESSION_TOKEN.
	SessionToken string `json:"session_token,omitempty"`
	// VersionID reads a specific version. Empty reads the current version.
	VersionID string `json:"version_id,omitempty"`
	// VersionStage reads a staging label, for example AWSPREVIOUS. Empty reads AWSCURRENT.
	VersionStage string `json:"version_stage,omitempty"`
}

// awsCredentials holds the resolved SigV4 signing credentials for one request.
type awsCredentials struct {
	// AccessKeyID is the access key.
	AccessKeyID string
	// SecretAccessKey is the secret key.
	SecretAccessKey string
	// SessionToken is an optional STS session token, empty for long-lived keys.
	SessionToken string
}

// resolveAWS reads a secret from AWS Secrets Manager over HTTP and returns its value, so a source
// resolves from Secrets Manager at run time with no aws CLI or SDK on the runner. It reads the JSON
// config, signs the GetSecretValue call with Signature Version 4, and returns the string value or
// the decoded binary value.
func resolveAWS(ctx context.Context, config string) (string, error) {
	var cfg awsConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: aws config is not valid JSON", ErrResolve)
	}
	if cfg.SecretID == "" {
		return "", fmt.Errorf("%w: aws config needs secret_id", ErrResolve)
	}
	creds, region, err := awsResolveCredentials(cfg)
	if err != nil {
		return "", err
	}

	endpoint := awsEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://%s.%s.amazonaws.com/", awsService, region)
	}
	if err := checkResolveURL(endpoint); err != nil {
		return "", err
	}

	payload := map[string]string{"SecretId": cfg.SecretID}
	if cfg.VersionID != "" {
		payload["VersionId"] = cfg.VersionID
	}
	if cfg.VersionStage != "" {
		payload["VersionStage"] = cfg.VersionStage
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Content-Type", awsContentType)
	req.Header.Set("X-Amz-Target", awsTarget)
	signAWSV4(req, body, creds, region, awsService, time.Now(), "x-amz-target")

	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: aws request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: aws returned %s", ErrResolve, resp.Status)
	}

	var out struct {
		SecretString string `json:"SecretString"`
		SecretBinary string `json:"SecretBinary"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("%w: aws response is not valid JSON", ErrResolve)
	}
	if out.SecretString != "" {
		return out.SecretString, nil
	}
	if out.SecretBinary != "" {
		decoded, err := base64.StdEncoding.DecodeString(out.SecretBinary)
		if err != nil {
			return "", fmt.Errorf("%w: aws binary secret is not valid base64", ErrResolve)
		}
		return string(decoded), nil
	}
	return "", fmt.Errorf("%w: aws secret has no value", ErrResolve)
}

// awsResolveCredentials fills the signing credentials and region from the config, falling back to
// the standard AWS environment variables for any empty field. It errors when the access key, secret
// key, or region cannot be found, since none can be assumed.
func awsResolveCredentials(cfg awsConfig) (awsCredentials, string, error) {
	access := firstNonEmpty(cfg.AccessKeyID, os.Getenv("AWS_ACCESS_KEY_ID"))
	secret := firstNonEmpty(cfg.SecretAccessKey, os.Getenv("AWS_SECRET_ACCESS_KEY"))
	session := firstNonEmpty(cfg.SessionToken, os.Getenv("AWS_SESSION_TOKEN"))
	region := firstNonEmpty(cfg.Region, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"))

	if access == "" || secret == "" {
		return awsCredentials{}, "", fmt.Errorf(
			"%w: aws needs access_key_id and secret_access_key in the config or the "+
				"AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment", ErrResolve)
	}
	if region == "" {
		return awsCredentials{}, "", fmt.Errorf(
			"%w: aws needs region in the config or the AWS_REGION environment", ErrResolve)
	}
	return awsCredentials{AccessKeyID: access, SecretAccessKey: secret, SessionToken: session}, region, nil
}

// firstNonEmpty returns the first non-empty string, or empty when all are empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// signAWSV4 sets the X-Amz-Date, optional X-Amz-Security-Token, and Authorization headers on req using
// AWS Signature Version 4 over the request body for the given service. It always signs content-type,
// host, and x-amz-date, adds x-amz-security-token when the credentials carry one, and signs any extra
// headers, such as x-amz-target for the JSON protocols, that the caller already set on req. The header
// names are sorted so the canonical request is deterministic, letting the JSON and query protocols
// share one signer.
func signAWSV4(req *http.Request, body []byte, creds awsCredentials, region, service string, now time.Time, extraSigned ...string) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	names := []string{"content-type", "host", "x-amz-date"}
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
		names = append(names, "x-amz-security-token")
	}
	names = append(names, extraSigned...)
	sort.Strings(names)

	var canonical strings.Builder
	for _, n := range names {
		value := req.Header.Get(n)
		if n == "host" {
			value = req.URL.Host
		}
		canonical.WriteString(n + ":" + value + "\n")
	}
	signed := strings.Join(names, ";")

	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method, uri, req.URL.RawQuery, canonical.String(), signed, sha256Hex(body),
	}, "\n")
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(awsSigningKey(creds.SecretAccessKey, dateStamp, region, service), stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+signature)
}

// awsSigningKey derives the SigV4 signing key by chaining HMAC-SHA256 over the date, region, and
// service, as AWS specifies.
func awsSigningKey(secretKey, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// hmacSHA256 returns the HMAC-SHA256 of data under key.
func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sha256Hex returns the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
