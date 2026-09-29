package util

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestMaskURL checks that only the scheme and host survive, so a path or query holding the
// credential never reaches a log or a response.
func TestMaskURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the raw URL.
		In string
		// WantMasked is the redaction.
		WantMasked string
	}{{ // Test 0: A webhook whose path is the credential.
		In:         "https://hooks.slack.com/services/T000/B000/XXXXSECRET",
		WantMasked: "https://hooks.slack.com/…",
	}, { // Test 1: A token in the query goes with the path.
		In:         "https://splunk.example:8088/services/collector/raw?token=SECRET",
		WantMasked: "https://splunk.example:8088/…",
	}, { // Test 2: Userinfo is not part of the host, so the password does not survive.
		In:         "https://user:hunter2@hooks.example/x",
		WantMasked: "https://hooks.example/…",
	}, { // Test 3: Empty stays empty; there is nothing to hide and nothing to show.
		In:         "",
		WantMasked: "",
	}, { // Test 4: A value naming no host is redacted whole.
		In:         "mailto:oncall@example.com",
		WantMasked: MaskMarker,
	}, { // Test 5: A value that does not parse is redacted whole.
		In:         "http://%zz/path",
		WantMasked: MaskMarker,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantMasked, MaskURL(test.In), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("MaskURL() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// errRefused stands in for the transport failure under a *url.Error, so a test can check the cause
// still matches after masking.
var errRefused = errors.New("connect: connection refused")

// TestMaskURLError checks that the address a *url.Error re-embeds in its message is redacted, along
// with any address the caller names, and that an error carrying no address is returned untouched.
func TestMaskURLError(t *testing.T) {
	t.Parallel()
	secret := "https://hooks.example/services/SECRETPATH"
	tests := []struct {
		// Name labels the shape under test.
		Name string
		// In is the error to mask.
		In error
		// URLs are the addresses the caller already knows about.
		URLs []string
		// WantMessage is the redacted message, empty when In is nil.
		WantMessage string
	}{{ // Test 0: A bare *url.Error, the shape net/http returns.
		Name:        "url error",
		In:          &url.Error{Op: "Post", URL: secret, Err: errRefused},
		WantMessage: `Post "https://hooks.example/…": connect: connection refused`,
	}, { // Test 1: Wrapped, since the caller adds its own context before logging.
		Name:        "wrapped url error",
		In:          fmt.Errorf("deliver: %w", &url.Error{Op: "Post", URL: secret, Err: errRefused}),
		WantMessage: `deliver: Post "https://hooks.example/…": connect: connection refused`,
	}, { // Test 2: Joined errors are followed into every branch.
		Name: "joined url errors",
		In: errors.Join(
			&url.Error{Op: "Post", URL: secret, Err: errRefused},
			&url.Error{Op: "Post", URL: "https://other.example/K3Y", Err: errRefused},
		),
		WantMessage: "Post \"https://hooks.example/…\": connect: connection refused\n" +
			"Post \"https://other.example/…\": connect: connection refused",
	}, { // Test 3: An error that quoted the address without being a *url.Error.
		Name:        "caller supplied address",
		In:          fmt.Errorf("giving up on %s after 2 tries", secret),
		URLs:        []string{secret},
		WantMessage: "giving up on https://hooks.example/… after 2 tries",
	}, { // Test 4: A longer address containing a shorter one is masked whole, not left with a tail.
		Name:        "overlapping addresses",
		In:          fmt.Errorf("post to https://hooks.example/a/LONGSECRET failed"),
		URLs:        []string{"https://hooks.example/a", "https://hooks.example/a/LONGSECRET"},
		WantMessage: "post to https://hooks.example/… failed",
	}, { // Test 5: Nothing to redact leaves the message alone.
		Name:        "no address",
		In:          errors.New("the collector is down"),
		WantMessage: "the collector is down",
	}, { // Test 6: A nil error masks to nil.
		Name: "nil",
		In:   nil,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := MaskURLError(test.In, test.URLs...)
			if test.In == nil {
				if got != nil {
					t.Fatalf("MaskURLError(nil) = %v, want nil", got)
				}
				return
			}
			if diff := cmp.Diff(test.WantMessage, got.Error()); diff != "" {
				t.Errorf("MaskURLError() message mismatch (-want +got):\n%s", diff)
			}
			if !errors.Is(got, test.In) {
				t.Errorf("MaskURLError() dropped the original error from the chain: %v", got)
			}
		})
	}
}

// TestMaskURLErrorKeepsTheCause pins that masking a message does not cost a caller the ability to
// tell one failure from another, since the error it stands for stays in the chain.
func TestMaskURLErrorKeepsTheCause(t *testing.T) {
	t.Parallel()
	in := &url.Error{Op: "Post", URL: "https://hooks.example/SECRET", Err: errRefused}
	got := MaskURLError(in)
	if !errors.Is(got, errRefused) {
		t.Errorf("errors.Is(got, errRefused) = false, want the cause reachable: %v", got)
	}
	var ue *url.Error
	if !errors.As(got, &ue) {
		t.Fatalf("errors.As(got, *url.Error) = false, want the url error reachable: %v", got)
	}
}
