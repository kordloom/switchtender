package util

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestMaskURLBoundaries pushes the webhook redactor onto the shapes a configured address can
// actually take. The whole address is the credential for a Slack, Discord, Teams, or Mattermost
// webhook, so anything that survives masking is something anyone reading a log line can post with.
// The rule is that only a scheme and a host may survive, and a value naming no host survives not at
// all.
func TestMaskURLBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the shape under test.
		Name string
		// In is the configured address.
		In string
		// WantResult is what may be shown.
		WantResult string
	}{{ // Test 0: A bare scheme names no host, so nothing about it is known to be safe.
		Name: "scheme only", In: "https://", WantResult: MaskMarker,
	}, { // Test 1: A path with no scheme and no host is redacted whole.
		Name: "path only", In: "/services/T000/SECRET", WantResult: MaskMarker,
	}, { // Test 2: A scheme relative address does carry a host, and only the host survives.
		Name: "scheme relative", In: "//hooks.example/services/SECRET", WantResult: "://hooks.example/…",
	}, { // Test 3: A port is part of the host and stays, since it identifies the collector.
		Name: "host and port", In: "https://splunk.example:8088/a/B",
		WantResult: "https://splunk.example:8088/…",
	}, { // Test 4: An IPv6 literal host survives with its brackets intact.
		Name: "ipv6 host", In: "https://[2001:db8::1]:8088/collector/TOK",
		WantResult: "https://[2001:db8::1]:8088/…",
	}, { // Test 5: A query with no path is still a place a token hides.
		Name: "query only", In: "https://hooks.example?token=SECRET",
		WantResult: "https://hooks.example/…",
	}, { // Test 6: So is a fragment.
		Name: "fragment", In: "https://hooks.example/a#SECRET", WantResult: "https://hooks.example/…",
	}, { // Test 7: Userinfo is neither the scheme nor the host, so a password does not survive.
		Name: "userinfo", In: "https://user:hunter2@hooks.example/x",
		WantResult: "https://hooks.example/…",
	}, { // Test 8: A trailing slash is all the path there is, and it goes like any other.
		Name: "root path", In: "https://hooks.example/", WantResult: "https://hooks.example/…",
	}, { // Test 9: No path at all still reads as a redaction rather than as an address to copy.
		Name: "no path", In: "https://hooks.example", WantResult: "https://hooks.example/…",
	}, { // Test 10: An uppercase scheme is normalized by the parser, and the host with it.
		Name: "uppercase", In: "HTTPS://Hooks.Example/SECRET", WantResult: "https://Hooks.Example/…",
	}, { // Test 11: A control byte makes the value unparseable, so it is redacted whole.
		Name: "control byte", In: "https://hooks.example/\x7f", WantResult: MaskMarker,
	}, { // Test 12: A value that is not an address at all names no host.
		Name: "not a url", In: "hooks.example/services/SECRET", WantResult: MaskMarker,
	}, { // Test 13: Empty stays empty: there is nothing to hide and nothing to show.
		Name: "empty", In: "", WantResult: "",
	}, { // Test 14: Whitespace names no host.
		Name: "whitespace", In: "   ", WantResult: MaskMarker,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := MaskURL(test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("MaskURL(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
			if strings.Contains(got, "SECRET") || strings.Contains(got, "hunter2") ||
				strings.Contains(got, "TOK") {
				t.Errorf("MaskURL(%q) = %q, which anyone reading a log can post with", test.In, got)
			}
		})
	}
}

// TestMaskURLOnAVeryLongAddress proves the redactor does not carry a long path through. A webhook
// address is configured by an operator, so its length is not this program's choice, and a path is
// exactly where a token sits.
func TestMaskURLOnAVeryLongAddress(t *testing.T) {
	t.Parallel()
	long := "https://hooks.example/" + strings.Repeat("S", 100000)
	got := MaskURL(long)
	if diff := cmp.Diff("https://hooks.example/…", got); diff != "" {
		t.Errorf("MaskURL() mismatch (-want +got):\n%s", diff)
	}
}

// TestMaskURLErrorBoundaries covers the ways an address reaches an error message, since masking the
// field a URL is logged in buys nothing if the error printed beside it carries the address anyway.
// The forwarder logs a delivery failure on every retry, so this is the line a token would leak in.
func TestMaskURLErrorBoundaries(t *testing.T) {
	t.Parallel()
	const secret = "https://hooks.example/services/SECRETPATH"
	tests := []struct {
		// Name labels the shape under test.
		Name string
		// In is the error to mask.
		In error
		// URLs are the addresses the caller already knows about.
		URLs []string
		// WantMessage is the redacted message.
		WantMessage string
	}{{ // Test 0: The same address named twice by the caller is masked once, not twice over.
		Name: "duplicate caller addresses", In: fmt.Errorf("giving up on %s", secret),
		URLs: []string{secret, secret}, WantMessage: "giving up on https://hooks.example/…",
	}, { // Test 1: An empty address in the caller's list is ignored rather than masking everything.
		Name: "empty address supplied", In: errors.New("the collector is down"),
		URLs: []string{""}, WantMessage: "the collector is down",
	}, { // Test 2: The caller names the same address the error already carries.
		Name: "caller repeats the url error's address",
		In:   &url.Error{Op: "Post", URL: secret, Err: errRefused}, URLs: []string{secret},
		WantMessage: `Post "https://hooks.example/…": connect: connection refused`,
	}, { // Test 3: A url error nested inside another is reached by unwrapping.
		Name: "doubly wrapped",
		In: fmt.Errorf("attempt 2: %w",
			fmt.Errorf("deliver: %w", &url.Error{Op: "Post", URL: secret, Err: errRefused})),
		WantMessage: `attempt 2: deliver: Post "https://hooks.example/…": connect: connection refused`,
	}, { // Test 4: A join holding one plain error and one url error is followed into both branches.
		Name: "joined with a plain error",
		In: errors.Join(errors.New("first sink refused"),
			&url.Error{Op: "Post", URL: secret, Err: errRefused}),
		WantMessage: "first sink refused\n" +
			`Post "https://hooks.example/…": connect: connection refused`,
	}, { // Test 5: An address appearing several times in one message is masked at every occurrence.
		Name: "repeated in the message",
		In:   fmt.Errorf("%s failed, retrying %s", secret, secret), URLs: []string{secret},
		WantMessage: "https://hooks.example/… failed, retrying https://hooks.example/…",
	}, { // Test 6: An address the message does not contain changes nothing.
		Name: "address not in the message", In: errors.New("timed out"),
		URLs: []string{secret}, WantMessage: "timed out",
	}, { // Test 7: A url error whose address is empty leaves the message alone.
		Name:        "url error with no address",
		In:          &url.Error{Op: "Post", URL: "", Err: errRefused},
		WantMessage: `Post "": connect: connection refused`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := MaskURLError(test.In, test.URLs...)
			if diff := cmp.Diff(test.WantMessage, got.Error()); diff != "" {
				t.Errorf("MaskURLError() message mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(got.Error(), "SECRETPATH") {
				t.Errorf("MaskURLError() leaked the credential path: %v", got)
			}
			if !errors.Is(got, test.In) {
				t.Errorf("MaskURLError() dropped the original from the chain: %v", got)
			}
		})
	}
}

// TestMaskURLErrorReturnsTheSameErrorWhenNothingChanged pins that an untouched message is handed
// back as the very error that came in rather than as a wrapper around it. A caller comparing errors
// by identity, and every type assertion that does not go through errors.As, keeps working only
// because of that.
func TestMaskURLErrorReturnsTheSameErrorWhenNothingChanged(t *testing.T) {
	t.Parallel()
	in := errors.New("the collector is down")
	if got := MaskURLError(in, "https://hooks.example/SECRET"); got != in {
		t.Errorf("MaskURLError() = %v (%T), want the identical error back", got, got)
	}
}
