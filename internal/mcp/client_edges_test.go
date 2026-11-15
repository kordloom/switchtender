package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewClientNormalizesAndRefusesServerAddresses pins the constructor's refusals and the one
// normalization it performs.
//
// A bare host becoming https rather than http is the security-relevant part: the operator's bearer
// token rides on every request this client makes, and a plain connection publishes it to anything on
// the path. Defaulting the other way would make an omission the thing that leaks the token.
func TestNewClientNormalizesAndRefusesServerAddresses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Base is the server address the operator configured.
		Base string
		// Token is the API token.
		Token string
		// WantBase is the normalized base, empty when the constructor must refuse.
		WantBase string
	}{
		{ // Test 0: A bare host is https, so a token never crosses a plain connection by omission.
			Name: "bare host", Base: "switchtender.internal", Token: "tok",
			WantBase: "https://switchtender.internal",
		},
		{ // Test 1: A bare host and port is https too.
			Name: "host and port", Base: "switchtender.internal:8443", Token: "tok",
			WantBase: "https://switchtender.internal:8443",
		},
		{ // Test 2: An explicit plain scheme is the operator's own decision and is honored.
			Name: "explicit http", Base: "http://localhost:8080", Token: "tok",
			WantBase: "http://localhost:8080",
		},
		{ // Test 3: Trailing slashes are trimmed so paths do not double up.
			Name: "trailing slashes", Base: "https://host///", Token: "tok", WantBase: "https://host",
		},
		{ // Test 4: Surrounding whitespace, which a copied value carries, is trimmed.
			Name: "padded", Base: "  https://host  ", Token: "tok", WantBase: "https://host",
		},
		{ // Test 5: A base path is kept, since an install can sit behind a prefix.
			Name: "base path", Base: "https://host/api", Token: "tok", WantBase: "https://host/api",
		},
		{ // Test 6: No address is a refusal, not a default.
			Name: "empty address", Base: "", Token: "tok",
		},
		{ // Test 7: Whitespace is no address either.
			Name: "whitespace address", Base: "   ", Token: "tok",
		},
		{ // Test 8: A scheme this client cannot speak is refused rather than attempted.
			Name: "ftp", Base: "ftp://host", Token: "tok",
		},
		{ // Test 9: So is a scheme that would read the local disk.
			Name: "file", Base: "file:///etc/passwd", Token: "tok",
		},
		{ // Test 10: And one that would run script in a host process.
			Name: "javascript", Base: "javascript://host", Token: "tok",
		},
		{ // Test 11: An address that will not parse is refused.
			Name: "unparsable", Base: "://host", Token: "tok",
		},
		{ // Test 12: No token is a refusal: an unauthenticated client is a client with no gate.
			Name: "empty token", Base: "https://host", Token: "",
		},
		{ // Test 13: A whitespace token is no token.
			Name: "whitespace token", Base: "https://host", Token: "  \t ",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c, err := NewClient(test.Base, test.Token, time.Second)
			if test.WantBase == "" {
				if err == nil {
					t.Fatalf("NewClient(%q, %q) was accepted, want a refusal", test.Base, test.Token)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClient(%q) error = %v", test.Base, err)
			}
			if c.base != test.WantBase {
				t.Errorf("base = %q, want %q", c.base, test.WantBase)
			}
			if c.token != test.Token {
				t.Errorf("token = %q, want the configured token", c.token)
			}
		})
	}
}

// TestNewClientRefusesAnAddressWithNoHost demonstrates a defect. The trailing-slash trim runs before
// the scheme test, so "https://" becomes "https:", which no longer contains "://" and is then treated
// as a bare host and prefixed again. The constructor accepts it and stores a base of
// "https://https:", so every tool call is aimed at a host named "https:" rather than the address
// being refused at startup where an operator would see the typo.
func TestNewClientRefusesAnAddressWithNoHost(t *testing.T) {
	t.Parallel()
	c, err := NewClient("https://", "tok", time.Second)
	if err == nil {
		t.Errorf("an address with no host was accepted and normalized to %q", c.base)
	}
}

// TestServerMessagePullsTheAPIsReasonOutOfAReply pins how a refusal reaches the model. The whole
// point of the API's message reaching an agent is that it can act on the reason, and the whole point
// of the clipping is that a proxy's HTML error page cannot flood a model's context in its place.
//
//nolint:funlen // Test function.
func TestServerMessagePullsTheAPIsReasonOutOfAReply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Data is the reply body.
		Data string
		// WantResult is the message handed to the model.
		WantResult string
	}{
		{ // Test 0: The API's own error field is the message.
			Name: "api error", Data: `{"error":"not authorized for this template"}`,
			WantResult: "not authorized for this template",
		},
		{ // Test 1: JSON without an error field falls back to the body.
			Name: "json without error", Data: `{"status":"nope"}`, WantResult: `{"status":"nope"}`,
		},
		{ // Test 2: An empty error field is no message, so the body is used.
			Name: "empty error field", Data: `{"error":""}`, WantResult: `{"error":""}`,
		},
		{ // Test 3: Plain text is used as it stands.
			Name: "plain text", Data: "upstream refused", WantResult: "upstream refused",
		},
		{ // Test 4: Only the first line survives, so a proxy's HTML page does not arrive whole.
			Name: "html page", Data: "<html>\n<head><title>502</title></head>\n<body>...</body>\n",
			WantResult: "<html>",
		},
		{ // Test 5: A single long line is clipped and says it was.
			Name: "long line", Data: strings.Repeat("x", 500),
			WantResult: strings.Repeat("x", 200) + "...",
		},
		{ // Test 6: Exactly the bound is not clipped, so the marker means something.
			Name: "exactly the bound", Data: strings.Repeat("x", 200),
			WantResult: strings.Repeat("x", 200),
		},
		{ // Test 7: An empty body still says something, rather than reading as a blank refusal.
			Name: "empty body", Data: "", WantResult: "no detail",
		},
		{ // Test 8: A whitespace body is empty too.
			Name: "whitespace body", Data: "  \n\t ", WantResult: "no detail",
		},
		{ // Test 9: A leading newline leaves nothing on the first line, which still reads as no detail.
			Name: "leading newline", Data: "\nreal message", WantResult: "real message",
		},
		{ // Test 10: A nested error object is not a string, so the body is used.
			Name: "nested error", Data: `{"error":{"code":7}}`, WantResult: `{"error":{"code":7}}`,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := serverMessage([]byte(test.Data)); got != test.WantResult {
				t.Errorf("serverMessage(%q) = %q, want %q", test.Name, got, test.WantResult)
			}
		})
	}
}

// TestAToolRefusalCarriesTheStatusAndTheReason pins that every non-2xx reply becomes an error naming
// both, across the statuses the product actually answers with. An approval gate holding a run and an
// authorization denial both arrive this way, and a model handed a bare status code cannot act on it.
func TestAToolRefusalCarriesTheStatusAndTheReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Status is what the API answers.
		Status int
		// Reason is the API's error text.
		Reason string
	}{
		{Status: http.StatusBadRequest, Reason: "template_id is required"},                 // Test 0.
		{Status: http.StatusUnauthorized, Reason: "the token was rejected"},                // Test 1.
		{Status: http.StatusForbidden, Reason: "not authorized for this template"},         // Test 2.
		{Status: http.StatusNotFound, Reason: "run not found"},                             // Test 3.
		{Status: http.StatusConflict, Reason: "the run is already terminal"},               // Test 4.
		{Status: http.StatusTooManyRequests, Reason: "slow down"},                          // Test 5.
		{Status: http.StatusInternalServerError, Reason: "could not collect the evidence"}, // Test 6.
		{Status: http.StatusServiceUnavailable, Reason: "the audit chain is not writable"}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.Status)
				_, _ = fmt.Fprintf(w, `{"error":%q}`, test.Reason)
			}))
			defer ts.Close()
			tool := findTool(t, Tools(testClient(t, ts), Options{}), "get_run")
			out, err := tool.Run(context.Background(), []byte(`{"run_id":"run_1"}`))
			if err == nil {
				t.Fatalf("get_run over a %d reply = %q, want a refusal", test.Status, out)
			}
			if !strings.Contains(err.Error(), test.Reason) {
				t.Errorf("error = %v, want the API's reason", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", test.Status)) {
				t.Errorf("error = %v, want the status named", err)
			}
		})
	}
}

// TestTheClientBoundsAResponseBody pins the memory bound on one reply. A run log can be far larger
// than an agent's context, and buffering it whole before deciding it was too big is the cost the
// bound exists to avoid.
func TestTheClientBoundsAResponseBody(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		chunk := strings.Repeat("a", 64<<10)
		for range 64 {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer ts.Close()

	tool := getRunLogTool(t, testClient(t, ts))
	out, err := tool.Run(context.Background(), []byte(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("get_run_log error = %v", err)
	}
	if len(out) > maxResponseBytes {
		t.Errorf("get_run_log returned %d bytes, above the %d bound", len(out), maxResponseBytes)
	}
	if len(out) == 0 {
		t.Error("get_run_log returned nothing for a large log")
	}
}

// TestAnUnreadableSuccessBodyIsAnError pins that a 200 carrying something other than the JSON the
// tool expects is reported rather than rendered as an empty result. An empty result would read to a
// model as a run with no fields, which is a different and worse answer than a decode failure.
func TestAnUnreadableSuccessBodyIsAnError(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>a proxy answered instead</html>"))
	}))
	defer ts.Close()
	tool := findTool(t, Tools(testClient(t, ts), Options{}), "get_run")
	if _, err := tool.Run(context.Background(), []byte(`{"run_id":"run_1"}`)); err == nil {
		t.Error("a non-JSON success body was rendered as a result")
	}
}

// TestAToolCallStopsWithItsContext pins that a canceled request does not keep running against the
// product after the client stopped waiting for it.
func TestAToolCallStopsWithItsContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := findTool(t, Tools(testClient(t, ts), Options{}), "list_templates")
	_, err := tool.Run(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the canceled context", err)
	}
}

// TestRequireAgentTokenFailsClosedWhenTheServerCannotBeReached pins the direction the startup check
// errs in. A probe that could not be made proves nothing about the token, so it must be its own
// error: never a pass, and never one of the two refusals that tell an operator what to change.
func TestRequireAgentTokenFailsClosedWhenTheServerCannotBeReached(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := testClient(t, ts)
	ts.Close()

	err := c.RequireAgentToken(context.Background())
	if err == nil {
		t.Fatal("an unreachable server was read as an agent token")
	}
	if errors.Is(err, ErrNotAgentToken) || errors.Is(err, ErrNoAccounts) {
		t.Errorf("error = %v, want an unreachable server reported as itself", err)
	}
	if !strings.Contains(err.Error(), "reach") {
		t.Errorf("error = %v, want it to say the server could not be reached", err)
	}
}

// TestRequireAgentTokenReadsOnlyAnAgentAnswerAsAnAgentToken pins every answer the startup check can
// receive. Only a 200 whose body names the agent actor type passes. A 200 that names anything else
// is a token that is not an agent's, a 200 that says the server is open is a server with no
// accounts, and every other status or unreadable body is its own error whatever its body claims.
func TestRequireAgentTokenReadsOnlyAnAgentAnswerAsAnAgentToken(t *testing.T) {
	t.Parallel()
	const agent = `{"actor_type":"agent"}`
	tests := []struct {
		// Want is the refusal the check must return, matched with errors.Is. Nil means the error,
		// if any, must be neither refusal.
		Want error
		// Name says what the case proves.
		Name string
		// Body is what the server answers with.
		Body string
		// Status is the status the server answers with.
		Status int
		// WantErr is whether any error is expected.
		WantErr bool
	}{
		{Name: "an agent answer passes", Status: http.StatusOK, Body: agent},
		{Name: "an empty object names no agent", Status: http.StatusOK, Body: `{}`,
			Want: ErrNotAgentToken, WantErr: true},
		{Name: "null names no agent", Status: http.StatusOK, Body: `null`,
			Want: ErrNotAgentToken, WantErr: true},
		{Name: "the actor type is matched exactly", Status: http.StatusOK,
			Body: `{"actor_type":"Agent"}`, Want: ErrNotAgentToken, WantErr: true},
		{Name: "a role named agent is not an actor type", Status: http.StatusOK,
			Body: `{"role":"agent","name":"agent"}`, Want: ErrNotAgentToken, WantErr: true},
		{Name: "open wins over an agent actor type", Status: http.StatusOK,
			Body: `{"actor_type":"agent","open":true}`, Want: ErrNoAccounts, WantErr: true},
		{Name: "a proxy page proves nothing", Status: http.StatusOK,
			Body: `<html>sign in</html>`, WantErr: true},
		{Name: "an empty body proves nothing", Status: http.StatusOK, WantErr: true},
		{Name: "a list proves nothing", Status: http.StatusOK, Body: `[` + agent + `]`,
			WantErr: true},
		{Name: "rejected token", Status: http.StatusUnauthorized, Body: agent, WantErr: true},
		{Name: "forbidden proves nothing", Status: http.StatusForbidden, Body: agent,
			WantErr: true},
		{Name: "a server without the endpoint", Status: http.StatusNotFound, Body: agent,
			WantErr: true},
		{Name: "created is not the answer", Status: http.StatusCreated, Body: agent,
			WantErr: true},
		{Name: "method refused proves nothing", Status: http.StatusMethodNotAllowed, Body: agent,
			WantErr: true},
		{Name: "rate limited proves nothing", Status: http.StatusTooManyRequests, Body: agent,
			WantErr: true},
		{Name: "gateway fault proves nothing", Status: http.StatusBadGateway, Body: agent,
			WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/auth/me" {
					t.Errorf("probed %q, want the endpoint that says who the caller is", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer st_test_token" {
					t.Errorf("the probe carried authorization %q, want the bearer token", got)
				}
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(test.Body))
			}))
			defer ts.Close()

			err := testClient(t, ts).RequireAgentToken(context.Background())
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: error = %v, want error: %v", test.Name, err, test.WantErr)
			}
			for _, refusal := range []error{ErrNotAgentToken, ErrNoAccounts} {
				if got, want := errors.Is(err, refusal), errors.Is(test.Want, refusal); got != want {
					t.Errorf("%s: errors.Is(%v) = %v, want %v (error was %v)",
						test.Name, refusal, got, want, err)
				}
			}
		})
	}
}

// TestARedirectIsARefusalRatherThanAReply pins that only a 2xx counts as an answer from the product.
//
// A 3xx the http client does not follow, a redirect carrying no Location or a 304, is handed back to
// doRaw as an ordinary response. Reading the success window as anything below 400 would accept those,
// and the damage lands hardest on the text endpoint: get_run_log does not decode what it receives, so
// a proxy's interstitial page would be returned to the model as the run's own output, with no status
// anywhere in it to say otherwise. A model cannot tell that from a log, and it is the record it would
// reason about and report. Every tool must refuse instead, naming the status.
func TestARedirectIsARefusalRatherThanAReply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Status is the redirect the proxy answers with.
		Status int
		// Body is what rides along, the page a model must never read as a reply.
		Body string
	}{
		{ // Test 0: A redirect with no Location is returned to the caller, not followed.
			Name: "found without location", Status: http.StatusFound,
			Body: "<html>moved: log in at the proxy</html>",
		},
		{ // Test 1: Multiple choices is never followed and carries a body of its own.
			Name: "multiple choices", Status: http.StatusMultipleChoices,
			Body: "<html>pick an endpoint</html>",
		},
		{ // Test 2: Not modified is not a redirect at all and carries no body to read.
			Name: "not modified", Status: http.StatusNotModified, Body: "",
		},
		{ // Test 3: The top of the redirect range, the boundary a wider window would swallow.
			Name: "permanent redirect", Status: http.StatusPermanentRedirect,
			Body: "<html>the api moved</html>",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(test.Body))
			}))
			defer ts.Close()

			client := testClient(t, ts)
			// The text endpoint first: it is the one that would hand the page straight to the model.
			out, err := getRunLogTool(t, client).Run(context.Background(), []byte(`{"run_id":"run_1"}`))
			if err == nil {
				t.Fatalf("get_run_log over a %d reply = %q, want a refusal", test.Status, out)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", test.Status)) {
				t.Errorf("get_run_log error = %v, want the %d status named", err, test.Status)
			}
			if test.Body != "" && strings.Contains(out, test.Body) {
				t.Errorf("get_run_log returned the redirect page as run output: %q", out)
			}

			// And the JSON endpoint, so the refusal is the client's and not a decode failure.
			out, err = findTool(t, Tools(client, Options{}), "get_run").
				Run(context.Background(), []byte(`{"run_id":"run_1"}`))
			if err == nil {
				t.Fatalf("get_run over a %d reply = %q, want a refusal", test.Status, out)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", test.Status)) {
				t.Errorf("get_run error = %v, want the %d status named", err, test.Status)
			}
		})
	}
}

// TestALongLogSaysItWasCutOff covers the difference between a bounded reply and a misleading one.
//
// A run log can be far larger than a model's context, so the client bounds what it returns. Bounding
// it silently is the problem: the model receives the first part of a long log as though it were the
// whole log, and a run whose failure and recap are at the end reads as a run that did nothing wrong.
// An agent triaging an incident then reports success.
//
// The reply says it was cut, and the bound still holds, because a notice that pushes the body past
// the limit defeats the reason the limit exists.
func TestALongLogSaysItWasCutOff(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for written := 0; written < maxResponseBytes+(1<<16); written += 1 << 12 {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1<<12))
		}
	}))
	defer ts.Close()

	c, err := NewClient(ts.URL, "st_test_token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	got, err := c.doText(context.Background(), http.MethodGet, "/v1/runs/run_1/logs")
	if err != nil {
		t.Fatalf("doText() error = %v", err)
	}
	if !strings.Contains(got, "cut off") {
		t.Error("a log longer than the bound came back with nothing to say it was cut, so a model " +
			"reads the part it was given as the whole run")
	}
	if len(got) > maxResponseBytes {
		t.Errorf("the reply is %d bytes, above the %d bound: the notice has to fit inside the "+
			"limit rather than be added on top of it", len(got), maxResponseBytes)
	}

	// A short reply is returned as it is, with nothing appended.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "all done\n")
	}))
	defer short.Close()
	sc, err := NewClient(short.URL, "st_test_token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	plain, err := sc.doText(context.Background(), http.MethodGet, "/v1/runs/run_1/logs")
	if err != nil {
		t.Fatalf("doText() error = %v", err)
	}
	if plain != "all done\n" {
		t.Errorf("a short log came back as %q, want it untouched", plain)
	}
}
