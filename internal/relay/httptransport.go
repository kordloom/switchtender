package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dcadolph/switchtender/internal/event"
	"github.com/dcadolph/switchtender/internal/run"
)

// httpTransport carries a worker's execution-path calls to a relay server over authenticated HTTP.
// It is the wire the relay Client runs against from an isolated segment: one outbound connection to
// the control node, the worker bearer token on every request, run.Run's own JSON tags on the body.
type httpTransport struct {
	// baseURL is the relay server's root, without a trailing slash.
	baseURL string
	// token is the worker bearer token presented on every call.
	token string
	// client issues the HTTP requests.
	client *http.Client
}

// compile-time proof that httpTransport is a Transport.
var _ Transport = (*httpTransport)(nil)

// NewHTTPTransport returns a Transport that carries the execution path to the relay server at
// baseURL, authenticating with the worker bearer token. A nil client uses http.DefaultClient. It
// panics on an empty base URL or token, both wiring errors in a worker.
func NewHTTPTransport(baseURL, token string, client *http.Client) Transport {
	if baseURL == "" {
		panic("relay: base URL required")
	}
	if token == "" {
		panic("relay: worker token required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &httpTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  client,
	}
}

// Claim leases the oldest pending run the owner's queues serve, mapping 204 to ErrNonePending.
func (t *httpTransport) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	resp, err := t.sendJSON(ctx, http.MethodPost, "/relay/v1/claim",
		claimRequest{Owner: owner, Queues: queues})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return decodeRun(resp.Body)
	case http.StatusNoContent:
		return nil, run.ErrNonePending
	default:
		return nil, statusErr("claim", resp)
	}
}

// Heartbeat renews the owner's lease on a run, mapping 404 to ErrNotFound.
func (t *httpTransport) Heartbeat(ctx context.Context, id, owner string) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, "/relay/v1/heartbeat",
		heartbeatRequest{ID: id, Owner: owner})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("heartbeat", resp)
}

// Get returns the run with the given id, mapping 404 to ErrNotFound.
func (t *httpTransport) Get(ctx context.Context, id string) (*run.Run, error) {
	resp, err := t.do(ctx, http.MethodGet, runPath(id, ""), "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return decodeRun(resp.Body)
	case http.StatusNotFound:
		return nil, run.ErrNotFound
	default:
		return nil, statusErr("get", resp)
	}
}

// Save writes the run's status transitions back to the control node.
func (t *httpTransport) Save(ctx context.Context, r *run.Run) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(r.ID, "/save"), r)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save", resp)
}

// AppendLog streams captured output to the control node, mapping 404 to ErrNotFound.
func (t *httpTransport) AppendLog(ctx context.Context, id string, p []byte) error {
	resp, err := t.do(ctx, http.MethodPost, runPath(id, "/log"),
		"application/octet-stream", bytes.NewReader(p))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("append log", resp)
}

// AppendEvents streams structured events to the control node, mapping 404 to ErrNotFound.
func (t *httpTransport) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(id, "/events"), events)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("append events", resp)
}

// SaveHostSummary records a run's per-host outcomes on the control node.
func (t *httpTransport) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(runID, "/host-summary"), summaries)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save host summary", resp)
}

// SaveTaskSummary records a run's per-task durations on the control node.
func (t *httpTransport) SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(runID, "/task-summary"), summaries)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save task summary", resp)
}

// sendJSON issues an authenticated request whose body is v encoded as JSON.
func (t *httpTransport) sendJSON(ctx context.Context, method, path string, v any) (*http.Response, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return t.do(ctx, method, path, "application/json", bytes.NewReader(buf))
}

// do issues an authenticated request with an optional body and returns the response for the caller
// to interpret. It is the one place the worker token is attached to the wire.
func (t *httpTransport) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	return resp, nil
}

// runPath builds a run-scoped relay path, escaping the id for safe placement in the URL.
func runPath(id, suffix string) string {
	return "/relay/v1/runs/" + url.PathEscape(id) + suffix
}

// decodeRun decodes a run from a response body.
func decodeRun(body io.Reader) (*run.Run, error) {
	var out run.Run
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	return &out, nil
}

// expectNoContent maps a 204 to success and any other status to an error, for the write calls that
// return no body.
func expectNoContent(op string, resp *http.Response) error {
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return statusErr(op, resp)
}

// noContentOr404 maps 204 to success and 404 to ErrNotFound, for the calls whose run may have been
// purged out from under the worker.
func noContentOr404(op string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return run.ErrNotFound
	default:
		return statusErr(op, resp)
	}
}

// statusErr builds an error from an unexpected response status, folding in any error message the
// relay server returned so a failed call names its cause.
func statusErr(op string, resp *http.Response) error {
	if msg := readErrorBody(resp.Body); msg != "" {
		return fmt.Errorf("%s: unexpected status %d: %s", op, resp.StatusCode, msg)
	}
	return fmt.Errorf("%s: unexpected status %d", op, resp.StatusCode)
}

// readErrorBody extracts the error field from a relay JSON error body, empty when it cannot.
func readErrorBody(body io.Reader) string {
	var e errorBody
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		return ""
	}
	return strings.TrimSpace(e.Error)
}
