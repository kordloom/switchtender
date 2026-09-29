// Package mcp serves the Model Context Protocol over stdio so an AI agent can propose infrastructure
// changes through SwitchTender instead of around it.
//
// The design rule is that this package holds no authority of its own. Every tool call is an ordinary
// authenticated request to the SwitchTender API, carrying an operator-bound token the agent was
// given, so it passes the same authorization gate, the same approval policy, and the same fail-closed
// audit append as a request from a person. An agent that talks to this server can therefore do
// exactly what its token permits and nothing more, and every change it proposes is recorded in the
// tamper-evident chain under its own name before it executes.
//
// What the server deliberately does not expose is as important as what it does. There is no approve
// or reject tool, so an agent can never release its own work; approval stays with a person out of
// band. There are no credential, user, token, grant, or policy tools, so an agent cannot widen its
// own reach. The surface is proposing runs and reading what happened.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds a response body read into memory. A run log can be large and an agent's
// context is not, so a reply is capped here rather than after it has been buffered.
const maxResponseBytes = 1 << 20

// ErrAdminToken is returned when the configured token carries admin rights. An agent must hold a
// scoped operator token, so the server refuses to start rather than lending an agent admin authority.
var ErrAdminToken = errors.New("the token has admin rights")

// Client calls the SwitchTender API on the agent's behalf. It is the only way this package reaches
// the product, which is what keeps the agent on the authenticated path.
type Client struct {
	// base is the API root, without a trailing slash, for example https://switchtender.internal.
	base string
	// token is the operator-bound bearer token every request carries.
	token string
	// http performs the requests.
	http *http.Client
}

// NewClient returns a Client for the server at base, authenticating with token. The base may be
// given with or without a scheme; a bare host is treated as https, since a token crossing a plain
// connection would be readable in transit.
func NewClient(base, token string, timeout time.Duration) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, errors.New("server address is required")
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("server address %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server address %q must be http or https", base)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("an API token is required")
	}
	return &Client{base: base, token: token, http: &http.Client{Timeout: timeout}}, nil
}

// doRaw performs one API call and returns the raw response body. A non-2xx reply becomes an error
// carrying the server's message, so an agent is told why it was refused rather than being handed a
// bare status code.
func (c *Client) doRaw(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", path, err)
	}
	defer func() { _ = res.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%s: %s", res.Status, serverMessage(data))
	}
	return data, nil
}

// do performs one API call and decodes the JSON reply into out, which may be nil to discard it.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	data, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// doText performs one API call and returns the response body as text, for an endpoint that serves
// text/plain rather than JSON, such as a run's log. Decoding that body as JSON, as do would, fails
// on the first character.
func (c *Client) doText(ctx context.Context, method, path string) (string, error) {
	data, err := c.doRaw(ctx, method, path, nil)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// serverMessage pulls the API's error text out of a reply, falling back to the raw body clipped to
// one line so a proxy's HTML page cannot flood an agent's context.
func serverMessage(data []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	text := strings.TrimSpace(string(data))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	if text == "" {
		return "no detail"
	}
	return text
}

// RefuseAdminToken checks the configured token's authority and returns ErrAdminToken when it is an
// admin token.
//
// The rule that an agent holds an operator-bound token is the difference between a governed agent and
// an agent that can approve its own work, and a rule enforced only in documentation is not enforced.
// The probe reads an admin-only endpoint: a token that may list accounts is an admin token, so the
// server refuses to start on it. A refusal on that endpoint is the healthy answer. An unreachable or
// unauthenticated server is reported as its own error rather than being read as either verdict.
func (c *Client) RefuseAdminToken(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/users", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.base, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBytes))

	switch res.StatusCode {
	case http.StatusOK:
		return ErrAdminToken
	case http.StatusForbidden, http.StatusNotFound:
		// Forbidden is the scoped token this server is meant to carry. Not found means the account
		// endpoints are not enabled on this install, which also means no admin authority was proven.
		return nil
	case http.StatusUnauthorized:
		return errors.New("the token was rejected by the server")
	default:
		return fmt.Errorf("checking token authority: %s", res.Status)
	}
}
