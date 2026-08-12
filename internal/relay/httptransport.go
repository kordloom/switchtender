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
	"github.com/kordloom/switchtender/internal/policy"
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
	// logAbandonAfter is how long a finished run's batch keeps retrying before it is dropped.
	//
	// A terminal run only released its batch when the final flush succeeded, and a failed flush
	// re-armed the retry timer, so a relay that stayed down left every finished run holding a live
	// timer and up to logBatchLimit of output, forever. A run that ended a quarter of an hour ago is
	// not going to deliver its tail, and holding it costs a long-lived worker unbounded memory.
	logAbandonAfter = 15 * time.Minute
	// logRetryBase is how long a batch waits after a failed post before another is attempted.
	//
	// Without it a relay that is down is asked once per write. A batch sitting at or above
	// logBatchBytes makes every subsequent append a size-triggered flush, so once the buffer is full
	// and posts are failing, one hundred small writes became one hundred synchronous failing
	// requests, each one paying a connection timeout on the execution path.
	logRetryBase = 250 * time.Millisecond
	// logRetryShift is how many times the retry wait doubles, so a relay that stays down is asked
	// every logRetryBase<<logRetryShift rather than continuously.
	logRetryShift = 5
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
	// mu guards batches and leases.
	mu sync.Mutex
	// batches holds each running run's buffered output, keyed by run id.
	batches map[string]*logBatch
	// leases holds the per-claim capability the control node minted for each run this worker holds,
	// keyed by run id. The claim response carries it once, and every report, record write, and
	// heartbeat this worker makes for that run presents it back. An entry is dropped when the run's
	// batch is, which is the last point anything could still be reported for the run. It is kept only
	// in memory and never written anywhere the run is serialized, matching the json:"-" on the field.
	leases map[string]string
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
	// nextRetry is when another post may be attempted after one failed. Zero means no failure is
	// outstanding.
	nextRetry time.Time
	// doneAt is when the run this batch belongs to reached a terminal state. Zero while it runs.
	doneAt time.Time
	// fails counts consecutive failed posts, which sets how long nextRetry waits.
	fails int
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
		leases:  make(map[string]string),
	}
}

// leaseFor returns the per-claim capability held for a run, or the empty string when none is held,
// which is the case for a run claimed from a control node that predates the capability.
func (t *httpTransport) leaseFor(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.leases[id]
}

// Claim leases the oldest pending run the owner's queues serve, mapping 204 to ErrNonePending.
func (t *httpTransport) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	resp, err := t.sendJSON(ctx, http.MethodPost, "/relay/v1/claim",
		claimRequest{Owner: owner, Queues: queues}, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		leased, derr := decodeRun(resp.Body)
		if derr != nil {
			return nil, derr
		}
		// The capability rode in on a header, not the body: the field is json:"-", so decodeRun never
		// saw it. It is kept in memory, keyed by run, and presented on every report made for this run.
		// A control node that predates the capability sends no header, and the run is reported the
		// older way.
		if lease := resp.Header.Get(leaseHeader); lease != "" {
			t.mu.Lock()
			t.leases[leased.ID] = lease
			t.mu.Unlock()
		}
		return leased, nil
	case http.StatusNoContent:
		return nil, run.ErrNonePending
	default:
		return nil, statusErr("claim", resp)
	}
}

// Heartbeat renews the owner's lease on a run, mapping 404 to ErrNotFound.
func (t *httpTransport) Heartbeat(ctx context.Context, id, owner string) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, "/relay/v1/heartbeat",
		heartbeatRequest{ID: id, Owner: owner}, t.leaseFor(id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("heartbeat", resp)
}

// Policies reads the approval policies in force on the control node.
func (t *httpTransport) Policies(ctx context.Context) ([]*policy.Policy, error) {
	resp, err := t.do(ctx, http.MethodGet, "/relay/v1/policies", "", nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("policies", resp)
	}
	var out []*policy.Policy
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode policies: %w", err)
	}
	return out, nil
}

// Get returns the run with the given id, mapping 404 to ErrNotFound.
func (t *httpTransport) Get(ctx context.Context, id string) (*run.Run, error) {
	resp, err := t.do(ctx, http.MethodGet, runPath(id, ""), "", nil, "")
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
	// A terminal run drops its batch, but only once there is nothing left in it. A failed final
	// flush puts its bytes back, and deleting the batch then would throw away the end of the run's
	// output even though the relay might recover a moment later.
	if r.Status.Terminal() {
		t.mu.Lock()
		if b := t.batches[r.ID]; b != nil {
			if b.doneAt.IsZero() {
				b.doneAt = time.Now()
			}
			// Drop the batch once it is empty. A finished run that is still failing to deliver its
			// tail is dropped by postFailed, once it has been trying for logAbandonAfter.
			if len(b.buf) == 0 {
				if b.timer != nil {
					b.timer.Stop()
					b.timer = nil
				}
				delete(t.batches, r.ID)
			}
		}
		t.mu.Unlock()
	}
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(r.ID, "/save"), r, t.leaseFor(r.ID))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := expectNoContent("save", resp); err != nil {
		return err
	}
	// The terminal save landed and is the last report a run makes for its own execution: summaries
	// and events are written while it is still running, before this. If nothing is still buffered,
	// the capability has no more reports to authorize and is dropped here, which also covers a run
	// that produced no output and so never had a batch to drop it with. A batch still retrying its
	// tail keeps the lease, and postFailed or postSucceeded drops it when that tail is delivered or
	// abandoned. The drop waits until the save succeeds so a retry after a failed save still holds it.
	if r.Status.Terminal() {
		t.mu.Lock()
		if _, ok := t.batches[r.ID]; !ok {
			delete(t.leases, r.ID)
		}
		t.mu.Unlock()
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
	// A full batch posts at once, unless a post just failed. While a relay is down every append
	// would otherwise be a size-triggered flush, since the buffer stays full once the bytes are put
	// back, turning each write into its own failing request on the execution path.
	now := time.Now()
	backingOff := now.Before(b.nextRetry)
	flushNow := len(b.buf) >= logBatchBytes && !backingOff
	if !flushNow && b.timer == nil {
		// Waiting out the backoff is itself a pending flush, so the batch is still posted once the
		// relay is worth asking again even if no further output arrives.
		delay := logBatchDelay
		if backingOff {
			delay = b.nextRetry.Sub(now)
		}
		b.timer = time.AfterFunc(delay, func() { t.flushLogAsync(id) })
	}
	t.mu.Unlock()
	if flushNow {
		// A failed flush has already put its bytes back at the front of the batch, so the next flush
		// carries them. Reporting the failure here would be reported to a caller that wraps this in
		// a retry, and that retry would append p a second time on top of output that was never lost.
		_ = t.flushLog(ctx, id)
	}
	// The append itself cannot fail once the bytes are buffered. A parked failure from an earlier
	// timer flush is not this call's to report, and returning it made the caller duplicate output.
	return nil
}

// flushLogAsync flushes a run's batch from its delay timer. No caller is waiting on this post, and a
// failure has already put its bytes back at the front of the batch, so the next flush carries them.
// The failure is not surfaced to a later AppendLog: that caller retries, and retrying an append
// whose bytes are already buffered duplicates them in the stored log.
func (t *httpTransport) flushLogAsync(id string) {
	_ = t.flushLog(context.Background(), id)
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
		t.postFailed(id)
		return err
	}
	t.postSucceeded(id)
	return nil
}

// postFailed opens or widens the batch's retry backoff and makes sure something is scheduled to try
// again, so a run that goes quiet after a failure still has its buffered output delivered once the
// relay returns.
func (t *httpTransport) postFailed(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.batches[id]
	if b == nil {
		return
	}
	b.fails++
	// A finished run stops retrying once it is clear the tail is not going to land. Re-arming
	// forever is what kept a dead relay's batches and their timers alive for the life of the worker.
	if !b.doneAt.IsZero() && time.Since(b.doneAt) > logAbandonAfter {
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		delete(t.batches, id)
		// The tail is abandoned, so nothing more will be reported for this run and the capability it
		// would have been reported under is dropped with the batch.
		delete(t.leases, id)
		return
	}
	wait := logRetryBase << min(b.fails-1, logRetryShift)
	b.nextRetry = time.Now().Add(wait)
	if b.timer == nil {
		b.timer = time.AfterFunc(wait, func() { t.flushLogAsync(id) })
	}
}

// postSucceeded clears the retry backoff, so a relay that comes back is not held off by the failures
// that preceded it, and drops the batch when a finished run has nothing left to send.
//
// Dropping it here matters because Save is the only other place that deletes, and Save has already
// run by the time a retry finally lands. A run that ended while the relay was briefly down left one
// map entry per run alive for the life of the worker.
func (t *httpTransport) postSucceeded(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.batches[id]
	if b == nil {
		return
	}
	b.fails = 0
	b.nextRetry = time.Time{}
	if !b.doneAt.IsZero() && len(b.buf) == 0 {
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		delete(t.batches, id)
		// The last of the run's output has landed, so no further report will be made for it and the
		// capability is dropped with the batch.
		delete(t.leases, id)
	}
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
		"application/octet-stream", bytes.NewReader(p), t.leaseFor(id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("append log", resp)
}

// AppendEvents streams structured events to the control node, mapping 404 to ErrNotFound.
func (t *httpTransport) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(id, "/events"), events, t.leaseFor(id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return noContentOr404("append events", resp)
}

// SaveHostSummary records a run's per-host outcomes on the control node.
func (t *httpTransport) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(runID, "/host-summary"), summaries, t.leaseFor(runID))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save host summary", resp)
}

// SaveHostFacts records the system facts a run gathered per host on the control node.
func (t *httpTransport) SaveHostFacts(ctx context.Context, runID string, facts []run.HostFacts) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(runID, "/host-facts"), facts, t.leaseFor(runID))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save host facts", resp)
}

// SaveTaskSummary records a run's per-task durations on the control node.
func (t *httpTransport) SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	resp, err := t.sendJSON(ctx, http.MethodPost, runPath(runID, "/task-summary"), summaries, t.leaseFor(runID))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return expectNoContent("save task summary", resp)
}

// sendJSON issues an authenticated request whose body is v encoded as JSON. A non-empty lease is
// presented as the per-claim capability for the run the request concerns.
func (t *httpTransport) sendJSON(ctx context.Context, method, path string, v any, lease string) (*http.Response, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return t.do(ctx, method, path, "application/json", bytes.NewReader(buf), lease)
}

// do issues an authenticated request with an optional body and returns the response for the caller
// to interpret. It is the one place the worker token, and the per-claim capability when one is given,
// are attached to the wire.
func (t *httpTransport) do(ctx context.Context, method, path, contentType string, body io.Reader, lease string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	if lease != "" {
		req.Header.Set(leaseHeader, lease)
	}
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
