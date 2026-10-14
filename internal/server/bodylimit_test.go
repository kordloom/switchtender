package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestRequestBodyLimit pins which writes may carry a body past the ordinary cap. An inventory a person
// imported can be larger than the ordinary cap, so creating and replacing one takes the import cap,
// and nothing else about inventories does.
func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Method is the request's method.
		Method string
		// Path is the request's path.
		Path string
		// Want is the most body bytes the request may carry.
		Want int64
	}{
		// Test 0: Creating an inventory takes the import cap.
		{Method: http.MethodPost, Path: "/v1/inventories", Want: maxUploadBodyBytes},
		// Test 1: Replacing an inventory takes the import cap.
		{Method: http.MethodPut, Path: "/v1/inventories/inv_1", Want: maxUploadBodyBytes},
		// Test 2: A case change does not escape or widen the match.
		{Method: http.MethodPut, Path: "/V1/Inventories/inv_1", Want: maxUploadBodyBytes},
		// Test 3: Deleting an inventory keeps the ordinary cap.
		{Method: http.MethodDelete, Path: "/v1/inventories/inv_1", Want: maxBodyBytes},
		// Test 4: A preview keeps the ordinary cap.
		{Method: http.MethodPost, Path: "/v1/inventories/preview", Want: maxBodyBytes},
		// Test 5: A saved inventory's preview keeps the ordinary cap.
		{Method: http.MethodPost, Path: "/v1/inventories/inv_1/preview", Want: maxBodyBytes},
		// Test 6: Clearing cached facts keeps the ordinary cap.
		{Method: http.MethodDelete, Path: "/v1/inventories/inv_1/facts/web1", Want: maxBodyBytes},
		// Test 7: A path that only starts like inventories keeps the ordinary cap.
		{Method: http.MethodPost, Path: "/v1/inventories-other", Want: maxBodyBytes},
		// Test 8: Another write keeps the ordinary cap.
		{Method: http.MethodPost, Path: "/v1/templates", Want: maxBodyBytes},
		// Test 9: An import keeps the upload cap it always had.
		{Method: http.MethodPost, Path: "/v1/import/awx", Want: maxUploadBodyBytes},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(test.Method, test.Path, nil)
			if got := requestBodyLimit(r); got != test.Want {
				t.Errorf("requestBodyLimit(%s %s) = %d, want %d", test.Method, test.Path, got, test.Want)
			}
		})
	}
}

// TestAnOversizedWriteNamesItsLimit sends bodies through the real cap and the audit gate's digest and
// checks the answer. The gate used to read the cap's own error as a failure to read, so a body over
// the limit was told only that it could not be read, with no number to stay under.
func TestAnOversizedWriteNamesItsLimit(t *testing.T) {
	t.Parallel()
	g := &authGate{log: zap.NewNop()}
	tests := []struct {
		// Method is the request's method.
		Method string
		// Path is the request's path.
		Path string
		// Size is the body's length in bytes.
		Size int
		// WantStatus is the answer's status.
		WantStatus int
		// WantMessage is text the refusal must contain, empty when the body is accepted.
		WantMessage string
	}{
		// Test 0: An inventory write a little over the ordinary cap is accepted.
		{Method: http.MethodPost, Path: "/v1/inventories", Size: maxBodyBytes + 4096, WantStatus: http.StatusOK},
		// Test 1: An inventory write past the import cap is refused with that cap named.
		{Method: http.MethodPost, Path: "/v1/inventories", Size: maxUploadBodyBytes + 4096,
			WantStatus: http.StatusRequestEntityTooLarge, WantMessage: "larger than the 26 MiB"},
		// Test 2: Another write a little over the ordinary cap is refused with that cap named.
		{Method: http.MethodPost, Path: "/v1/templates", Size: maxBodyBytes + 4096,
			WantStatus: http.StatusRequestEntityTooLarge, WantMessage: "larger than the 1 MiB"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := bodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, _, ok := g.digestBody(w, r); ok {
					w.WriteHeader(http.StatusOK)
				}
			}))
			r := httptest.NewRequest(test.Method, test.Path, bytes.NewReader(bytes.Repeat([]byte("a"), test.Size)))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.WantStatus {
				t.Errorf("status = %d, want %d (body %s)", w.Code, test.WantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), test.WantMessage) {
				t.Errorf("answer %q does not say %q", strings.TrimSpace(w.Body.String()), test.WantMessage)
			}
		})
	}
}
