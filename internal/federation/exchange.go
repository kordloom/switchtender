package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/safedial"
)

// exchangeTimeout bounds one call to a cloud token service, so an unreachable endpoint fails the
// run's setup instead of holding its worker slot.
const exchangeTimeout = 30 * time.Second

// maxExchangeBody caps how much of a token service's reply is read.
const maxExchangeBody = 1 << 20

// maxErrorDetail caps how much of a token service's error message reaches the run's recorded error.
const maxErrorDetail = 300

// exchangeClient sends tokens to cloud token services. It refuses redirects, so a token is never
// re-sent to a host a redirect chose, and its dialer refuses the cloud metadata addresses, so an
// endpoint override cannot aim a run's token at the instance credentials service.
var exchangeClient = &http.Client{
	Timeout:       exchangeTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     safedial.Transport(),
}

// awsSTSVersion is the STS query API version.
const awsSTSVersion = "2011-06-15"

// awsSessionCredentials are the temporary credentials AssumeRoleWithWebIdentity returns.
type awsSessionCredentials struct {
	// AccessKeyID is the temporary access key id.
	AccessKeyID string `xml:"AccessKeyId"`
	// SecretAccessKey is the temporary secret key.
	SecretAccessKey string `xml:"SecretAccessKey"`
	// SessionToken is the session token that goes with them.
	SessionToken string `xml:"SessionToken"`
}

// awsAssumeResponse is the part of the AssumeRoleWithWebIdentity reply that carries credentials.
type awsAssumeResponse struct {
	// Credentials holds the session credentials.
	Credentials awsSessionCredentials `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
}

// awsErrorResponse is an STS error reply.
type awsErrorResponse struct {
	// Code is the error code, such as AccessDenied or InvalidIdentityToken.
	Code string `xml:"Error>Code"`
	// Message explains the code.
	Message string `xml:"Error>Message"`
}

// awsSTSEndpoint returns the STS endpoint for cfg: the override, the regional endpoint, or the
// global one.
func awsSTSEndpoint(cfg Config) string {
	switch {
	case cfg.STSEndpoint != "":
		return cfg.STSEndpoint
	case cfg.Region != "":
		return "https://sts." + cfg.Region + ".amazonaws.com/"
	default:
		return "https://sts.amazonaws.com/"
	}
}

// exchangeAWS calls AssumeRoleWithWebIdentity. The call is unsigned by design: the token is the
// authentication, so no AWS key of any kind is needed or stored to make it.
func exchangeAWS(ctx context.Context, cfg Config, token, session string) (awsSessionCredentials, error) {
	form := url.Values{}
	form.Set("Action", "AssumeRoleWithWebIdentity")
	form.Set("Version", awsSTSVersion)
	form.Set("RoleArn", cfg.RoleARN)
	form.Set("RoleSessionName", session)
	form.Set("WebIdentityToken", token)
	if cfg.SessionDuration > 0 {
		form.Set("DurationSeconds", strconv.Itoa(cfg.SessionDuration))
	}
	body, status, err := post(ctx, awsSTSEndpoint(cfg), "application/x-www-form-urlencoded",
		[]byte(form.Encode()), "")
	if err != nil {
		return awsSessionCredentials{}, fmt.Errorf("%w: aws sts: %v", ErrExchange, err)
	}
	if status != http.StatusOK {
		var e awsErrorResponse
		_ = xml.Unmarshal(body, &e)
		return awsSessionCredentials{}, fmt.Errorf("%w: aws sts answered %d %s", ErrExchange, status,
			errorDetail(e.Code, e.Message, token))
	}
	var out awsAssumeResponse
	if err := xml.Unmarshal(body, &out); err != nil {
		return awsSessionCredentials{}, fmt.Errorf("%w: aws sts reply is not valid XML", ErrExchange)
	}
	c := out.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return awsSessionCredentials{}, fmt.Errorf("%w: aws sts returned no credentials", ErrExchange)
	}
	return c, nil
}

// gcpSTSResponse is Google's token exchange reply.
type gcpSTSResponse struct {
	// AccessToken is the federated access token.
	AccessToken string `json:"access_token"`
	// Error is the OAuth error code on failure.
	Error string `json:"error"`
	// ErrorDescription explains the error.
	ErrorDescription string `json:"error_description"`
}

// gcpImpersonateRequest asks the IAM credentials service for a service account access token.
type gcpImpersonateRequest struct {
	// Scope lists the scopes the token is for.
	Scope []string `json:"scope"`
}

// gcpImpersonateResponse is the IAM credentials service's reply.
type gcpImpersonateResponse struct {
	// AccessToken is the service account's access token.
	AccessToken string `json:"accessToken"`
	// Error carries a failure.
	Error struct {
		// Status is the error status, such as PERMISSION_DENIED.
		Status string `json:"status"`
		// Message explains the status.
		Message string `json:"message"`
	} `json:"error"`
}

// exchangeGCP trades the token at Google's security token service for a federated access token,
// then, when the credential names a service account, trades that for the service account's own
// token.
func exchangeGCP(ctx context.Context, cfg Config, token string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("audience", cfg.Provider)
	form.Set("scope", gcpCloudPlatformScope)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token_type", gcpJWTTokenType)
	form.Set("subject_token", token)
	body, status, err := post(ctx, cfg.STSEndpoint, "application/x-www-form-urlencoded",
		[]byte(form.Encode()), "")
	if err != nil {
		return "", fmt.Errorf("%w: google sts: %v", ErrExchange, err)
	}
	var sts gcpSTSResponse
	_ = json.Unmarshal(body, &sts)
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: google sts answered %d %s", ErrExchange, status,
			errorDetail(sts.Error, sts.ErrorDescription, token))
	}
	if sts.AccessToken == "" {
		return "", fmt.Errorf("%w: google sts returned no access token", ErrExchange)
	}
	if cfg.ServiceAccount == "" {
		return sts.AccessToken, nil
	}
	req, err := json.Marshal(gcpImpersonateRequest{Scope: []string{gcpCloudPlatformScope}})
	if err != nil {
		return "", fmt.Errorf("%w: encode impersonation request: %v", ErrExchange, err)
	}
	body, status, err = post(ctx, impersonationURL(cfg), "application/json", req, sts.AccessToken)
	if err != nil {
		return "", fmt.Errorf("%w: google iam credentials: %v", ErrExchange, err)
	}
	var imp gcpImpersonateResponse
	_ = json.Unmarshal(body, &imp)
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: google iam credentials answered %d %s", ErrExchange, status,
			errorDetail(imp.Error.Status, imp.Error.Message, token, sts.AccessToken))
	}
	if imp.AccessToken == "" {
		return "", fmt.Errorf("%w: google iam credentials returned no access token", ErrExchange)
	}
	return imp.AccessToken, nil
}

// post sends body to endpoint and returns the reply body and status. A non-empty bearer is sent as
// the Authorization header. A transport error is reported without the URL's query, which a token
// never travels in but an operator's override could carry anything in.
func post(ctx context.Context, endpoint, contentType string, body []byte, bearer string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json, text/xml")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := exchangeClient.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, 0, uerr.Err
		}
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read reply: %w", err)
	}
	return reply, resp.StatusCode, nil
}

// errorDetail renders a token service's error code and message for the run's recorded error, capped
// in length and with every one of secrets cut out of it. A service does not echo the token it
// refused, but this is the one string that leaves the exchange for the run record, so it is not
// trusted to.
func errorDetail(code, message string, secrets ...string) string {
	detail := strings.Trim(strings.TrimSpace(code+": "+message), ": ")
	for _, s := range secrets {
		if s != "" {
			detail = strings.ReplaceAll(detail, s, "***")
		}
	}
	if len(detail) > maxErrorDetail {
		detail = detail[:maxErrorDetail]
	}
	if detail == "" {
		return "with no detail"
	}
	return "(" + detail + ")"
}
