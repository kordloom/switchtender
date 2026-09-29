package witness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTheReadTokenGateCannotBeSteppedAroundByPath pins that a spelling of a guarded path which is
// not literally prefixed "/witness/" cannot reach a guarded route's body.
//
// The gate matches on the request path before the mux normalizes it, which is the shape a gate is
// usually walked around: "//witness/findings" and "/foo/../witness/findings" are not prefixed
// "/witness/", so the gate does not fire on them. What keeps this closed is that the mux answers
// the un-normalized spelling with a redirect rather than the route, and the redirect target is
// gated. That is a property of the two layers together, so it is pinned here rather than assumed.
func TestTheReadTokenGateCannotBeSteppedAroundByPath(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa")})
	id := testIdentity(t)
	const token = "s3cret-read-token"
	s, err := NewService(id, t.TempDir(), time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client(),
		WithServiceClock(func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }),
		WithServiceReadToken(token))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(s.Close)
	s.CheckAll(context.Background())

	api := httptest.NewServer(s.Handler())
	// Registered as cleanup rather than deferred: the subtests below are parallel, so they run
	// after this function returns and a deferred close would have shut the server before them.
	t.Cleanup(api.Close)

	// Redirects are not followed, so a route answered only after a redirect to the canonical path
	// is visible as a redirect here rather than as a success the gate never saw.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	spellings := []string{
		"/witness/findings",
		"//witness/findings",
		"/./witness/findings",
		"/foo/../witness/findings",
		"/witness//findings",
		"/witness/servers",
		"//witness/servers",
	}
	for testNum, path := range spellings {
		t.Run(fmt.Sprintf("test %d %s", testNum, path), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, api.URL+path, nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode == http.StatusOK {
				t.Errorf("%s answered 200 without a token, so the gate was stepped around", path)
			}
		})
	}

	// The control: the canonical path with the token does answer, so the test above is not passing
	// because nothing works.
	req, err := http.NewRequest(http.MethodGet, api.URL+"/witness/findings", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("get with token: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the guarded route with a valid token = %d, want 200", res.StatusCode)
	}
}
