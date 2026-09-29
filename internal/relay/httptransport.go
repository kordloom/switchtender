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
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

const (
	// logBatchBytes is how much output the transport buffers for one run before posting it. A batch
	// that reaches this size posts immediately rather than waiting out logBatchDelay.
	logBatchBytes = 64 << 10
	// logBatchDelay is how long a partial batch waits for more output before posting anyway, so a
	// quiet run's last lines still reach the control node promptly enough to tail.
	logBatchDelay = 250 * time.Millisecond
	// logBatchLimit caps the output held for one run while posts are failing, past which the oldest
	// is dropped rather than grown against a relay that is not coming back.
	logBatchLimit = 8 * logBatchBytes
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
	// mu guards batches.
	mu sync.Mutex
	// batches holds each running run's buffered output, keyed by run id.
	batches map[string]*logBatch
}

// logBatch accumulates one run's output between posts. A tool writes output in small chunks, so
// posting each one would cost a request per chunk; coalescing bounds that by size and by time.
type logBatch struct {
	// send serializes the posts for this run. Taking buf under it makes post order match take
	// order, so a size-triggered flush cannot overtake a timer-triggered one and scramble the log.
	send sync.Mutex
	// buf holds the output written but not yet posted.
	buf []byte
	// timer fires the delayed flush of a partial batch, nil when no flush is pending.
	timer *time.Timer
	// err is the failure from a flush no caller was waiting on, returned by the next call so a
	// broken relay still surfaces rather than silently dropping output.
	err error
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
		batches: make(map[string]*logBatch),
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

// Save writes the run's status transitions back to the control node. Buffered output is posted
// first so the log a status change closes off is already complete, and a run reaching a terminal
// state drops its batch, which is the last point anything could still be buffered for it.
func (t *httpTransport) Save(ctx context.Context, r *run.Run) error {
	flushErr := t.flushLog(ctx, r.ID)
	if r.Status.Terminal() {
		t.mu.Lock()
		delete(t.batches, r.ID)
		t.mu.Unlock()
	}
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(r.ID, "/save"), r)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := expectNoContent("save", resp); err != nil {
		return err
	}
	return flushErr
}

// AppendLog buffers captured output and posts it once it reaches logBatchBytes or has waited
// logBatchDelay, so a run writing many small chunks costs a request per batch rather than one per
// chunk. Save flushes whatever is left, so nothing is stranded when a run ends.
func (t *httpTransport) AppendLog(ctx context.Context, id string, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	t.mu.Lock()
	b := t.batches[id]
	if b == nil {
		b = &logBatch{}
		t.batches[id] = b
	}
	b.buf = append(b.buf, p...)
	full := len(b.buf) >= logBatchBytes
	pending := b.err
	b.err = nil
	if !full && b.timer == nil {
		b.timer = time.AfterFunc(logBatchDelay, func() { t.flushLogAsync(id) })
	}
	t.mu.Unlock()
	if full {
		if err := t.flushLog(ctx, id); err != nil {
			return err
		}
	}
	return pending
}

// flushLogAsync flushes a run's batch from its delay timer and parks any failure on the batch, since
// no caller is waiting on this post. The next AppendLog or Save returns it.
func (t *httpTransport) flushLogAsync(id string) {
	if err := t.flushLog(context.Background(), id); err != nil {
		t.mu.Lock()
		if b := t.batches[id]; b != nil {
			b.err = err
		}
		t.mu.Unlock()
	}
}

// flushLog posts a run's buffered output, mapping 404 to ErrNotFound. It is a no-op when the run
// has nothing buffered.
func (t *httpTransport) flushLog(ctx context.Context, id string) error {
	t.mu.Lock()
	b := t.batches[id]
	t.mu.Unlock()
	if b == nil {
		return nil
	}

	// Taking the buffer under send makes the order posts are issued in the order output was taken.
	b.send.Lock()
	defer b.send.Unlock()
	t.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	out := b.buf
	b.buf = nil
	t.mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	if err := t.postLog(ctx, id, out); err != nil {
		t.requeue(id, out)
		return err
	}
	return nil
}

// requeue puts unsent output back at the front of the batch so the next flush carries it, which is
// what makes a caller's retry of a failed append mean anything. Output past logBatchLimit is dropped
// oldest first, so a relay that stays down cannot grow the buffer without bound.
func (t *httpTransport) requeue(id string, out []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.batches[id]
	if b == nil {
		return
	}
	b.buf = append(out, b.buf...)
	if excess := len(b.buf) - logBatchLimit; excess > 0 {
		b.buf = b.buf[excess:]
	}
}

// postLog sends one batch of output to the control node, mapping 404 to ErrNotFound.
func (t *httpTransport) postLog(ctx context.Context, id string, p []byte) error {
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
