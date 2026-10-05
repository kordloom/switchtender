package run

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
)

// requireStorable fails the test when key is not text PostgreSQL stores.
func requireStorable(t *testing.T, what, key string) {
	t.Helper()
	if !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		t.Errorf("%s = %q, which PostgreSQL refuses in a text column", what, key)
	}
}

// TestDerivedKeysAreTextEveryBackendStores covers the keys the server derives from ids that are not
// text: a provisioning callback joins its template and its host with a NUL byte, and an inventory
// host name can carry a byte that is not UTF-8. Each must come out as text PostgreSQL stores, still
// ending with its window number, and two different ids must never share a key.
func TestDerivedKeysAreTextEveryBackendStores(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0)
	bucket := strconv.FormatInt(at.UnixNano()/int64(DedupeWindow), 10)
	tests := []struct {
		Name         string
		ID           string
		WantVerbatim bool
	}{{ // Test 0: A plain id stands in the key as it is.
		Name: "plain", ID: "run_a", WantVerbatim: true,
	}, { // Test 1: A template and a host joined by a NUL byte.
		Name: "nul tuple", ID: "tpl_cb\x00web01",
	}, { // Test 2: The same template with another host is another key.
		Name: "nul tuple other host", ID: "tpl_cb\x00web02",
	}, { // Test 3: A host name that is not UTF-8.
		Name: "invalid utf8", ID: "tpl_cb\x00web\xff",
	}, { // Test 4: An id spelled like a digest is digested too, so it cannot pose as one.
		Name: "digest mark", ID: digestMark + digestHex("tpl_cb\x00web01"),
	}, { // Test 5: Unicode is text, so it stands in the key as it is.
		Name: "unicode", ID: "tpl_cb:wéb", WantVerbatim: true,
	}}
	seen := map[string]string{}
	for testNum, test := range tests {
		key := DedupeKey("callback", test.ID, at)
		requireStorable(t, fmt.Sprintf("test %d %s: DedupeKey()", testNum, test.Name), key)
		if !strings.HasSuffix(key, ":"+bucket) || !strings.HasPrefix(key, internalKeyPrefix) {
			t.Errorf("test %d %s: DedupeKey() = %q, want the reserved prefix and the window %s",
				testNum, test.Name, key, bucket)
		}
		if verbatim := strings.Contains(key, ":"+test.ID+":"); verbatim != test.WantVerbatim {
			t.Errorf("test %d %s: DedupeKey() = %q, verbatim %v, want %v", testNum, test.Name, key,
				verbatim, test.WantVerbatim)
		}
		if other, dup := seen[key]; dup {
			t.Errorf("test %d %s: DedupeKey() = %q, the key of %q too", testNum, test.Name, key,
				other)
		}
		seen[key] = test.Name
		// at opens a window, so the last instant of that window derives the same key.
		if again := DedupeKey("callback", test.ID, at.Add(DedupeWindow-time.Nanosecond)); again != key {
			t.Errorf("test %d %s: the same id in the same window derived %q then %q", testNum,
				test.Name, key, again)
		}
	}
}

// TestClientKeysAreTextEveryBackendStores covers the keys a caller's Idempotency-Key header is
// stored under. A key scoped to an organization used to be the organization and the key joined by
// a NUL byte, and a header can carry bytes that are not UTF-8. Both must be stored as text
// PostgreSQL holds, the same key must keep deriving the same stored key, and a key an install
// without organizations sends as plain text keeps its shape.
func TestClientKeysAreTextEveryBackendStores(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Supplied     string
		OrgID        string
		WantVerbatim bool
	}{{ // Test 0: A plain key with no organization is stored as sent.
		Name: "plain", Supplied: "deploy-2026-10-04", WantVerbatim: true,
	}, { // Test 1: A key scoped to an organization.
		Name: "scoped", Supplied: "deploy-2026-10-04", OrgID: "org_a",
	}, { // Test 2: A key that is not UTF-8, as a header can carry it.
		Name: "invalid utf8", Supplied: "deploy\xff",
	}, { // Test 3: The same under an organization.
		Name: "invalid utf8 scoped", Supplied: "deploy\xff", OrgID: "org_a",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := ClientKey(test.Supplied, test.OrgID)
			if err != nil {
				t.Fatalf("ClientKey() error = %v", err)
			}
			requireStorable(t, "ClientKey()", got)
			if verbatim := got == test.Supplied; verbatim != test.WantVerbatim {
				t.Errorf("ClientKey() = %q, verbatim %v, want %v", got, verbatim, test.WantVerbatim)
			}
			again, err := ClientKey(test.Supplied, test.OrgID)
			if err != nil || again != got {
				t.Errorf("the same key derived %q then %q, %v: a retry would fire a second run",
					got, again, err)
			}
		})
	}
}

// TestCurrentKeyFindsWhatAnEarlierReleaseStored covers the keys earlier releases stored that
// PostgreSQL cannot hold. SQLite kept them, so an install upgrading in place has runs under them,
// and each must be rewritten to exactly the key this release derives for the same request, or a
// retried submission and a repeated callback stop finding the run they already made.
func TestCurrentKeyFindsWhatAnEarlierReleaseStored(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0)
	bucket := strconv.FormatInt(at.UnixNano()/int64(DedupeWindow), 10)
	scoped, err := ClientKey("deploy-2026-10-04", "org_a")
	if err != nil {
		t.Fatalf("ClientKey() error = %v", err)
	}
	invalid, err := ClientKey("deploy\xff", "")
	if err != nil {
		t.Fatalf("ClientKey() error = %v", err)
	}
	tests := []struct {
		Name        string
		Stored      string
		WantKey     string
		WantChanged bool
	}{{ // Test 0: A key scoped to an organization, as earlier releases joined it.
		Name: "scoped", Stored: "org_a\x00deploy-2026-10-04", WantKey: scoped, WantChanged: true,
	}, { // Test 1: A provisioning callback's replay key, as earlier releases derived it.
		Name:    "callback",
		Stored:  "st:callback:tpl_cb\x00web01:" + bucket,
		WantKey: DedupeKey("callback", "tpl_cb\x00web01", at), WantChanged: true,
	}, { // Test 2: A callback for a host named with a colon keeps its window.
		Name:    "callback host with colon",
		Stored:  "st:callback:tpl_cb\x00fe80::1:" + bucket,
		WantKey: DedupeKey("callback", "tpl_cb\x00fe80::1", at), WantChanged: true,
	}, { // Test 3: A caller's key that was not UTF-8, stored as it arrived.
		Name: "invalid utf8", Stored: "deploy\xff", WantKey: invalid, WantChanged: true,
	}, { // Test 4: A key that is already text is left alone.
		Name: "plain", Stored: "deploy-2026-10-04", WantKey: "deploy-2026-10-04",
	}, { // Test 5: A derived key that is already text is left alone.
		Name: "derived plain", Stored: DedupeKey("rerun", "run_a", at),
		WantKey: DedupeKey("rerun", "run_a", at),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, changed := CurrentKey(test.Stored)
			if diff := cmp.Diff(test.WantKey, got); diff != "" {
				t.Errorf("CurrentKey() mismatch (-want +got):\n%s", diff)
			}
			if changed != test.WantChanged {
				t.Errorf("CurrentKey() changed = %v, want %v", changed, test.WantChanged)
			}
			requireStorable(t, "CurrentKey()", got)
		})
	}

	// A reserved key in no shape this release derives still comes out as text, and as a key no
	// caller can send.
	odd, changed := CurrentKey("st:odd\x00")
	requireStorable(t, "CurrentKey(st:odd)", odd)
	if _, err := ClientKey(odd, ""); !changed || err == nil {
		t.Errorf("CurrentKey(st:odd) = %q, changed %v: want a rewritten key no caller can send",
			odd, changed)
	}
}
