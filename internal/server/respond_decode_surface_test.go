package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
)

// TestWantsPretty pins how the pretty query parameter is read. Indented JSON is a display choice,
// so the rule has to be predictable: presence alone turns it on, and only the exact word false
// turns it off. A reader that treated any value as false would silently ignore "pretty=1".
func TestWantsPretty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Query      string
		WantPretty bool
	}{{ // Test 0: No parameter at all means compact, the documented default.
		Name: "absent", Query: "", WantPretty: false,
	}, { // Test 1: A bare flag with no value turns it on.
		Name: "bare flag", Query: "?pretty", WantPretty: true,
	}, { // Test 2: An empty value still counts as present.
		Name: "empty value", Query: "?pretty=", WantPretty: true,
	}, { // Test 3: The literal word false is the one way to turn it off explicitly.
		Name: "false", Query: "?pretty=false", WantPretty: false,
	}, { // Test 4: Any other value is on, including a numeric one.
		Name: "one", Query: "?pretty=1", WantPretty: true,
	}, { // Test 5: The comparison is exact, so a capitalized False is not the off switch.
		Name: "capital false", Query: "?pretty=False", WantPretty: true,
	}, { // Test 6: The word true is on, the obvious case.
		Name: "true", Query: "?pretty=true", WantPretty: true,
	}, { // Test 7: An unrelated parameter leaves it off.
		Name: "other parameter", Query: "?format=json", WantPretty: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/runs"+test.Query, nil)
			if got := wantsPretty(req); got != test.WantPretty {
				t.Errorf("%s: wantsPretty = %v, want %v", test.Name, got, test.WantPretty)
			}
		})
	}
}

// TestRespondJSONNeverCaches pins the no-store header on every JSON reply. API responses carry the
// account roster, token metadata, and credential descriptions, and without this a browser or a
// proxy may write them to a disk cache that outlives the session and is readable afterward from a
// shared machine's profile directory.
func TestRespondJSONNeverCaches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Status     int
		Value      any
		Pretty     bool
		WantBody   string
		WantStatus int
	}{{ // Test 0: An ordinary compact success.
		Name: "compact", Status: http.StatusOK, Value: map[string]string{"a": "b"},
		WantBody: `{"a":"b"}`, WantStatus: http.StatusOK,
	}, { // Test 1: The indented shape carries the same header and the same status.
		Name: "pretty", Status: http.StatusCreated, Value: map[string]string{"a": "b"},
		Pretty: true, WantStatus: http.StatusCreated,
	}, { // Test 2: An error body is a JSON reply too, so it is not cached either.
		Name: "error body", Status: http.StatusForbidden, Value: errorResponse{Error: "forbidden"},
		WantBody: `{"error":"forbidden"}`, WantStatus: http.StatusForbidden,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			respondJSON(rec, zap.NewNop(), test.Status, test.Value, test.Pretty)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d", test.Name, rec.Code, test.WantStatus)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: Cache-Control = %q, want no-store", test.Name, got)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("%s: Content-Type = %q, want the JSON content type", test.Name, got)
			}
			if test.WantBody != "" {
				if diff := cmp.Diff(test.WantBody, strings.TrimSpace(rec.Body.String())); diff != "" {
					t.Errorf("%s: body mismatch (-want +got):\n%s", test.Name, diff)
				}
			}
		})
	}
}

// TestRespondJSONFallsBackWhenTheValueCannotBeMarshaled pins that an unmarshalable value produces a
// server error with a JSON body rather than a half-written response. A handler that hands this
// function a channel or a cyclic structure is a bug, but the caller must still get an answer it can
// parse rather than an empty body with a 200 already on the wire.
func TestRespondJSONFallsBackWhenTheValueCannotBeMarshaled(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	respondJSON(rec, zap.NewNop(), http.StatusOK, make(chan int), false)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &body); err != nil {
		t.Fatalf("fallback body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Error("fallback body carries no error message")
	}
}

// TestAcceptsGzip pins the content negotiation, including the explicit refusal by weight. A client
// that says gzip;q=0 is refusing gzip, and compressing for it anyway hands an unreadable body to a
// client that told us plainly it could not read one.
func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Header     string
		WantAccept bool
	}{{ // Test 0: No header at all means no gzip.
		Name: "absent", Header: "", WantAccept: false,
	}, { // Test 1: The plain announcement.
		Name: "gzip", Header: "gzip", WantAccept: true,
	}, { // Test 2: One entry among several is still an announcement.
		Name: "among others", Header: "deflate, gzip, br", WantAccept: true,
	}, { // Test 3: Surrounding whitespace is trimmed.
		Name: "padded", Header: "  gzip  ", WantAccept: true,
	}, { // Test 4: The token is matched case-insensitively, as the header grammar allows.
		Name: "uppercase", Header: "GZIP", WantAccept: true,
	}, { // Test 5: A zero weight is an explicit refusal.
		Name: "q zero", Header: "gzip;q=0", WantAccept: false,
	}, { // Test 6: A zero weight written with decimals is the same refusal.
		Name: "q zero decimals", Header: "gzip;q=0.000", WantAccept: false,
	}, { // Test 7: A non-zero weight is an acceptance.
		Name: "q half", Header: "gzip;q=0.5", WantAccept: true,
	}, { // Test 8: A weight of one is an acceptance.
		Name: "q one", Header: "gzip;q=1.0", WantAccept: true,
	}, { // Test 9: Another encoding alone is not gzip.
		Name: "br only", Header: "br", WantAccept: false,
	}, { // Test 10: A wildcard is not treated as gzip, so nothing is compressed on a guess.
		Name: "wildcard", Header: "*", WantAccept: false,
	}, { // Test 11: A refused gzip earlier in the list wins over anything after it, because the
		// first gzip entry is the one that answers.
		Name: "refused then other", Header: "gzip;q=0, deflate", WantAccept: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
			if test.Header != "" {
				req.Header.Set("Accept-Encoding", test.Header)
			}
			if got := acceptsGzip(req); got != test.WantAccept {
				t.Errorf("%s: acceptsGzip = %v, want %v", test.Name, got, test.WantAccept)
			}
		})
	}
}

// TestGzipResponseFlushReleasesWhatIsHeld pins that a handler which flushes gets its bytes out
// immediately rather than waiting for a kilobyte. Holding back what a handler explicitly pushed is
// a stall, and a stalled progressive response is worse than an uncompressed one.
func TestGzipResponseFlushReleasesWhatIsHeld(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	gz := &gzipResponse{ResponseWriter: rec}
	gz.WriteHeader(http.StatusAccepted)
	if _, err := gz.Write([]byte("first chunk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body escaped before the flush: %q", rec.Body.String())
	}
	gz.Flush()
	if got := rec.Body.String(); got != "first chunk" {
		t.Errorf("flushed body = %q, want the held bytes verbatim", got)
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want the handler's 202", rec.Code)
	}
	// After a flush the wrapper is in pass-through, so later writes go straight out uncompressed.
	if _, err := gz.Write([]byte(" and more")); err != nil {
		t.Fatalf("write after flush: %v", err)
	}
	gz.finish()
	if got := rec.Body.String(); got != "first chunk and more" {
		t.Errorf("final body = %q, want both chunks uncompressed", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none on a flushed pass-through", got)
	}
}

// TestGzipResponseUnwrapReachesTheRealWriter pins that Unwrap returns the writer underneath, which
// is what lets an http.ResponseController reach the real connection to set a deadline or flush. A
// wrapper that hid the connection would break every handler that needs one.
func TestGzipResponseUnwrapReachesTheRealWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	gz := &gzipResponse{ResponseWriter: rec}
	if got := gz.Unwrap(); got != http.ResponseWriter(rec) {
		t.Errorf("Unwrap returned %T, want the recorder underneath", got)
	}
}

// TestGzipResponseKeepsTheFirstStatus pins that only the first WriteHeader is recorded and that a
// later one cannot change it. A handler that writes an error after starting a body must not be able
// to relabel a response whose status is already settled.
func TestGzipResponseKeepsTheFirstStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	gz := &gzipResponse{ResponseWriter: rec}
	gz.WriteHeader(http.StatusCreated)
	gz.WriteHeader(http.StatusInternalServerError)
	gz.finish()
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want the first one written, 201", rec.Code)
	}
}

// TestGzipResponseLeavesAnAlreadyEncodedBodyAlone pins that a handler which set its own
// Content-Encoding is not encoded a second time. Double encoding produces a body no client can
// read, and the prepared assets are exactly this case.
func TestGzipResponseLeavesAnAlreadyEncodedBodyAlone(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	gz := &gzipResponse{ResponseWriter: rec}
	gz.Header().Set("Content-Encoding", "gzip")
	payload := bytes.Repeat([]byte("x"), gzipMinSize+64)
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	gz.finish()
	if got := rec.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Errorf("body was re-encoded: got %d bytes, want the %d handed in", len(got), len(payload))
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want the handler's own gzip untouched", got)
	}
}

// TestGzipResponseCompressesPastTheThresholdAndDropsContentLength pins the two things a compressed
// reply must get right: the body round-trips through gunzip to exactly what the handler wrote, and
// the handler's Content-Length is removed because it counts bytes that are no longer what goes out.
// A stale Content-Length on a compressed body truncates the reply at the client.
func TestGzipResponseCompressesPastTheThresholdAndDropsContentLength(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	gz := &gzipResponse{ResponseWriter: rec}
	payload := bytes.Repeat([]byte("switchtender "), 200)
	gz.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	gz.Header().Set("Content-Type", "application/json")
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	gz.finish()
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip past the threshold", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it dropped once the body is encoded", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open gzip reader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the decompressed body is not what the handler wrote")
	}
}

// TestDecodeErrorMessage pins what a caller is told about a rejected body. An unknown field is
// named, because somebody who misspelled a safety control needs to know which word was wrong, and
// everything else is the generic message so the parser's internals are not narrated back. The cap
// matters too: an enormous field name must not turn into an enormous error body.
func TestDecodeErrorMessage(t *testing.T) {
	t.Parallel()
	longName := strings.Repeat("a", 500)
	tests := []struct {
		Name        string
		Body        string
		Target      any
		WantMessage string
		WantNamed   bool
	}{{ // Test 0: A truncated body says nothing about the parser.
		Name: "truncated", Body: `{`, WantMessage: badBodyMessage,
	}, { // Test 1: A body that is not an object at all gets the same generic answer.
		Name: "array", Body: `[1,2,3]`, WantMessage: badBodyMessage,
	}, { // Test 2: The wrong type in a declared field is also generic.
		Name: "wrong type", Body: `{"name":42}`, WantMessage: badBodyMessage,
	}, { // Test 3: An empty body is a decode failure with the generic answer.
		Name: "empty", Body: ``, WantMessage: badBodyMessage,
	}, { // Test 4: An unknown field is named so the caller can find the typo.
		Name: "unknown field", Body: `{"nmae":"ops"}`, WantNamed: true, //nolint:misspell // The typo is the fixture: it is the unknown field under test.
	}, { // Test 5: A very long unknown field name is clipped, so a megabyte of key cannot become a
		// megabyte of error body.
		Name: "very long unknown field", Body: `{"` + longName + `":1}`, WantNamed: true,
	}, { // Test 6: A unicode field name is reported without being cut mid-rune.
		Name: "unicode unknown field", Body: `{"ünbekannt":1}`, WantNamed: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var dst createTeamRequest
			err := strictDecode(strings.NewReader(test.Body), &dst)
			if err == nil {
				t.Fatalf("%s: decode accepted %q", test.Name, test.Body)
			}
			got := decodeErrorMessage(err)
			if test.WantNamed {
				if !strings.HasPrefix(got, "unknown field ") {
					t.Errorf("%s: message = %q, want it to name the field", test.Name, got)
				}
				if len(got) > unknownFieldNameCap+64 {
					t.Errorf("%s: message is %d bytes, want it clipped near the %d cap",
						test.Name, len(got), unknownFieldNameCap)
				}
				return
			}
			if diff := cmp.Diff(test.WantMessage, got); diff != "" {
				t.Errorf("%s: message mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestDecodeStrictOptionalTreatsAnAbsentBodyAsNoOverrides pins the one relaxation in the strict
// rule: a handler whose body may be absent reads nothing sent as no overrides, while a body that is
// present is held to exactly the same standard. Blurring those two would let a malformed body pass
// as if nothing had been sent.
func TestDecodeStrictOptionalTreatsAnAbsentBodyAsNoOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Body       string
		NilBody    bool
		WantOK     bool
		WantName   string
		WantStatus int
	}{{ // Test 0: A nil reader leaves the destination untouched and is accepted.
		Name: "nil body", NilBody: true, WantOK: true, WantName: "before",
	}, { // Test 1: An empty body is end of input, which is also no overrides.
		Name: "empty body", Body: "", WantOK: true, WantName: "before",
	}, { // Test 2: A present body is decoded and does override.
		Name: "present body", Body: `{"name":"after"}`, WantOK: true, WantName: "after",
	}, { // Test 3: A malformed body that is present is still refused, not read as absent.
		Name: "malformed body", Body: `{"name":`, WantOK: false, WantName: "before",
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: An unknown field in an optional body is refused the same as in a required one.
		Name: "unknown field", Body: `{"nmae":"after"}`, WantOK: false, WantName: "before", //nolint:misspell // The typo is the fixture: it is the unknown field under test.
		WantStatus: http.StatusBadRequest,
	}, { // Test 5: Whitespace alone is end of input, so it is no overrides rather than malformed.
		Name: "whitespace only", Body: "   \n\t ", WantOK: true, WantName: "before",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dst := createTeamRequest{Name: "before"}
			rec := httptest.NewRecorder()
			var body io.Reader
			if !test.NilBody {
				body = strings.NewReader(test.Body)
			}
			got := decodeStrictOptional(rec, zap.NewNop(), body, &dst)
			if got != test.WantOK {
				t.Errorf("%s: accepted = %v, want %v", test.Name, got, test.WantOK)
			}
			if dst.Name != test.WantName {
				t.Errorf("%s: name = %q, want %q", test.Name, dst.Name, test.WantName)
			}
			if test.WantStatus != 0 && rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d", test.Name, rec.Code, test.WantStatus)
			}
		})
	}
}

// TestDecodeForeignStaysLenient pins the single named exception to the strict rule. A webhook
// delivery, a vendor export, and a model's reply are written by somebody else and gain fields
// without asking us, so refusing them on an unknown key would break a push the moment the sender
// added a field.
func TestDecodeForeignStaysLenient(t *testing.T) {
	t.Parallel()
	var dst struct {
		// Ref is the branch reference a push delivery carries.
		Ref string `json:"ref"`
	}
	payload := `{"ref":"refs/heads/main","a_field_added_next_year":{"deep":[1,2]}}`
	if err := decodeForeign([]byte(payload), &dst); err != nil {
		t.Fatalf("foreign payload refused: %v", err)
	}
	if dst.Ref != "refs/heads/main" {
		t.Errorf("ref = %q, want refs/heads/main", dst.Ref)
	}
	// Malformed is still malformed: leniency is about unknown keys, not about broken JSON.
	if err := decodeForeign([]byte(`{`), &dst); err == nil {
		t.Error("a truncated foreign payload was accepted")
	}
}
