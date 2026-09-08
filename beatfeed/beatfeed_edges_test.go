package beatfeed

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestBeatRoundTripsEveryShapeTheFeedCanCarry pins that a beat survives encode and decode
// unchanged at the edges of what a producer can put on the wire. The witness is out of tree and
// builds against this struct alone, so a value that does not round-trip is a value the witness
// silently misreads, and misreading a beat is indistinguishable from the tail removal the feed
// exists to expose.
func TestBeatRoundTripsEveryShapeTheFeedCanCarry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which edge the beat sits on.
		Name string
		// In is the beat a producer encodes.
		In Beat
	}{{ // Test 0: The zero beat, which is what a decoder produces from an empty object.
		Name: "zero", In: Beat{},
	}, { // Test 1: The first beat a chain ever mints.
		Name: "first", In: Beat{Beat: 1, At: "2026-08-10T11:50:00Z", Seq: 1, Head: "00"},
	}, { // Test 2: The largest numbers the fields can hold, which a long-lived chain approaches.
		Name: "max", In: Beat{
			Beat: math.MaxInt64, At: "9999-12-31T23:59:59Z", Seq: math.MaxInt64,
			Head: strings.Repeat("f", 64),
		},
	}, { // Test 3: Negative numbers, which are never minted but must not be mangled into something
		// a witness reads as valid.
		Name: "negative", In: Beat{Beat: -1, At: "2026-08-10T11:50:00Z", Seq: -9, Head: "abc"},
	}, { // Test 4: A time with an offset rather than Z, which RFC 3339 allows.
		Name: "offset", In: Beat{Beat: 3, At: "2026-08-10T06:50:00-05:00", Seq: 4, Head: "ab"},
	}, { // Test 5: Non-ASCII bytes in the free-text fields, which must not break the encoding.
		Name: "unicode", In: Beat{Beat: 5, At: "2026-08-10T11:50:00Z", Seq: 6, Head: "hé…🚂"},
	}, { // Test 6: A very long head, the shape a corrupt or hostile producer sends.
		Name: "long head", In: Beat{
			Beat: 7, At: "2026-08-10T11:50:00Z", Seq: 8, Head: strings.Repeat("a", 64<<10),
		},
	}, { // Test 7: Empty strings, which a decoder must keep empty rather than defaulting.
		Name: "empty strings", In: Beat{Beat: 9, Seq: 10},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.In)
			if err != nil {
				t.Fatalf("Marshal(%s) error = %v", test.Name, err)
			}
			var back Beat
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.In, back, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s did not round-trip (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestBeatAlwaysCarriesAllFourFields pins that no field is omitted at its zero value. A witness
// tells a quiet chain from a truncated one by reading beat numbers, and a producer that dropped
// "beat": 0 from the wire would hand the witness a beat it decodes as zero either way, so an absent
// field and a present zero must never be the same bytes.
func TestBeatAlwaysCarriesAllFourFields(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Beat{})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"at", "beat", "head", "seq"}
	if diff := cmp.Diff(want, keys, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the beat's field set drifted (-want +got):\n%s", diff)
	}
}

// TestBeatDecodeRefusesAWrongType pins that the feed's decoder fails closed on a value of the wrong
// shape rather than reading it as a zero. A beat number silently decoded as zero would make a
// witness read a live chain as one that never beat, which is the alarm this contract exists to
// raise, so a malformed feed must be an error and not a quiet zero.
func TestBeatDecodeRefusesAWrongType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what is wrong with the document.
		Name string
		// Raw is the JSON a decoder is handed.
		Raw string
		// WantDecodeError says the decode must fail rather than produce a zero value.
		WantDecodeError bool
		// WantBeat is the beat a successful decode must produce.
		WantBeat Beat
	}{{ // Test 0: A beat number sent as a string, which must not read as beat zero.
		Name: "string beat", Raw: `{"beat":"7","at":"x","seq":1,"head":"a"}`, WantDecodeError: true,
	}, { // Test 1: A sequence sent as a float, which must not silently truncate.
		Name: "float seq", Raw: `{"beat":1,"at":"x","seq":1.5,"head":"a"}`, WantDecodeError: true,
	}, { // Test 2: A head sent as a number, which must not read as an empty head.
		Name: "number head", Raw: `{"beat":1,"at":"x","seq":1,"head":9}`, WantDecodeError: true,
	}, { // Test 3: Truncated JSON, the shape a cut connection produces.
		Name: "truncated", Raw: `{"beat":1,"at":`, WantDecodeError: true,
	}, { // Test 4: Nulls decode to the zero value, which JSON defines and a witness may see.
		Name: "nulls", Raw: `{"beat":null,"at":null,"seq":null,"head":null}`, WantBeat: Beat{},
	}, { // Test 5: A field the witness does not know is ignored, so a server that adds one does not
		// break every watcher built against an older copy of this contract.
		Name: "unknown field", Raw: `{"beat":2,"at":"t","seq":3,"head":"h","cadence_s":60}`,
		WantBeat: Beat{Beat: 2, At: "t", Seq: 3, Head: "h"},
	}, { // Test 6: A missing field stays at its zero value rather than failing the whole beat.
		Name: "missing fields", Raw: `{"beat":4}`, WantBeat: Beat{Beat: 4},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got Beat
			err := json.Unmarshal([]byte(test.Raw), &got)
			if test.WantDecodeError {
				if err == nil {
					t.Fatalf("%s decoded without error into %+v, so a malformed feed reads as a "+
						"valid beat", test.Name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s failed to decode: %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantBeat, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestFeedPathsAgreeWithTheAuthCarveOut pins the relationship between the two path constants, not
// only their literal values. The server's auth carve-out matches FeedPath after trimming the
// version prefix from the request path, while the witness builds its URL from APIPath, so the day
// the two stop composing is the day the feed either goes unreachable or the carve-out opens a route
// nobody meant to leave open.
func TestFeedPathsAgreeWithTheAuthCarveOut(t *testing.T) {
	t.Parallel()
	if got := strings.TrimPrefix(APIPath, "/v1"); got != FeedPath {
		t.Errorf("trimming the version prefix from APIPath gives %q, want FeedPath %q", got, FeedPath)
	}
	if APIPath != "/v1"+FeedPath {
		t.Errorf("APIPath = %q, want %q", APIPath, "/v1"+FeedPath)
	}
	if !strings.HasPrefix(FeedPath, "/") {
		t.Errorf("FeedPath = %q, want a rooted path so the carve-out compares whole segments", FeedPath)
	}
	// A trailing slash, a query, or a wildcard would each make the carve-out match more than the one
	// route it is meant to open.
	for _, bad := range []string{"?", "*", "{", " "} {
		if strings.Contains(FeedPath, bad) {
			t.Errorf("FeedPath = %q, want no %q in a path an auth carve-out compares exactly",
				FeedPath, bad)
		}
	}
	if strings.HasSuffix(FeedPath, "/") {
		t.Errorf("FeedPath = %q, want no trailing slash", FeedPath)
	}
	if LimitParam == "" {
		t.Error("LimitParam is empty, so no request can bound how many beats it asks for")
	}
}

// TestFeedContractLiteralsAreStable pins the exact strings a deployed witness already hardcodes.
// An out-of-tree consumer built against an earlier copy of this package cannot be recompiled by
// changing this repository, so these values are frozen and a change here has to be a deliberate
// break rather than a rename that looked local.
func TestFeedContractLiteralsAreStable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name identifies the constant.
		Name string
		// Got is the constant's current value.
		Got string
		// WantValue is the value every deployed consumer already assumes.
		WantValue string
	}{
		{Name: "FeedPath", Got: FeedPath, WantValue: "/audit/beats"},  // Test 0: The open route.
		{Name: "APIPath", Got: APIPath, WantValue: "/v1/audit/beats"}, // Test 1: The full path.
		{Name: "LimitParam", Got: LimitParam, WantValue: "limit"},     // Test 2: The bound.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantValue, test.Got); diff != "" {
				t.Errorf("%s drifted (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}
