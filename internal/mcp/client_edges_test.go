package mcp

import (
	"context"
	"errors"
	"fmt"
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

// TestRefuseAdminTokenFailsClosedWhenTheServerCannotBeReached pins the direction the startup check
// errs in. It exists to stop this server lending an agent admin authority, and a probe that could
// not be made proves nothing about the token, so it must be an error rather than a pass.
func TestRefuseAdminTokenFailsClosedWhenTheServerCannotBeReached(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := testClient(t, ts)
	ts.Close()

	err := c.RefuseAdminToken(context.Background())
	if err == nil {
		t.Fatal("an unreachable server was read as a token without admin rights")
	}
	if errors.Is(err, ErrAdminToken) {
		t.Error("an unreachable server was read as an admin token, which is the other wrong verdict")
	}
	if !strings.Contains(err.Error(), "reach") {
		t.Errorf("error = %v, want it to say the server could not be reached", err)
	}
}

// TestRefuseAdminTokenReadsEveryStatusAsOneVerdict pins the whole status table of the startup check.
// Only a 200 on the admin-only endpoint proves admin rights, only 403 and 404 prove the token is
// scoped, and every other answer is its own error rather than being folded into either verdict.
func TestRefuseAdminTokenReadsEveryStatusAsOneVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Status is what the admin-only probe endpoint answers.
		Status int
		// WantAdmin is whether the token must be judged an admin token.
		WantAdmin bool
		// WantErr is whether any error is expected.
		WantErr bool
	}{
		{Name: "listing accounts is admin", Status: http.StatusOK, WantAdmin: true, WantErr: true},
		{Name: "forbidden is the scoped token", Status: http.StatusForbidden},
		{Name: "accounts not enabled", Status: http.StatusNotFound},
		{Name: "rejected token", Status: http.StatusUnauthorized, WantErr: true},
		{Name: "no content proves nothing", Status: http.StatusNoContent, WantErr: true},
		{Name: "method refused proves nothing", Status: http.StatusMethodNotAllowed, WantErr: true},
		{Name: "rate limited proves nothing", Status: http.StatusTooManyRequests, WantErr: true},
		{Name: "gateway fault proves nothing", Status: http.StatusBadGateway, WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/users" {
					t.Errorf("probed %q, want the admin-only account list", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer st_test_token" {
					t.Errorf("the probe carried authorization %q, want the bearer token", got)
				}
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(strings.Repeat("x", 4<<10)))
			}))
			defer ts.Close()

			err := testClient(t, ts).RefuseAdminToken(context.Background())
			if got := errors.Is(err, ErrAdminToken); got != test.WantAdmin {
				t.Errorf("ErrAdminToken = %v, want %v (error was %v)", got, test.WantAdmin, err)
			}
			if (err != nil) != test.WantErr {
				t.Errorf("error = %v, want error: %v", err, test.WantErr)
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
