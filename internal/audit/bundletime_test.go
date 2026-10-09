package audit

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestParseBundleTimeReadsOnlyTheOneForm holds the built-in verifier to the LoomSeal format's one
// time form, including each spelling time.Parse reads and the reference verifier refuses.
func TestParseBundleTimeReadsOnlyTheOneForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantTime time.Time
		In       string
		WantErr  bool
	}{{ // Test 0: Whole seconds in UTC.
		In: "2026-07-27T15:00:00Z", WantTime: time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC),
	}, { // Test 1: A fraction is read to the microsecond, the rest dropped.
		In:       "2026-07-27T15:00:00.123456789Z",
		WantTime: time.Date(2026, 7, 27, 15, 0, 0, 123456000, time.UTC),
	}, { // Test 2: The first year allowed.
		In: "0001-01-01T00:00:00Z", WantTime: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
	}, { // Test 3: A numeric offset, even one of zero.
		In: "2026-07-27T15:00:00+00:00", WantErr: true,
	}, { // Test 4: A space in place of the T.
		In: "2026-07-27 15:00:00Z", WantErr: true,
	}, { // Test 5: Year 0000.
		In: "0000-07-27T15:00:00Z", WantErr: true,
	}, { // Test 6: A lower case z.
		In: "2026-07-27T15:00:00z", WantErr: true,
	}, { // Test 7: A lower case t.
		In: "2026-07-27t15:00:00Z", WantErr: true,
	}, { // Test 8: A one-digit hour.
		In: "2026-07-27T5:00:00Z", WantErr: true,
	}, { // Test 9: A comma before the fraction.
		In: "2026-07-27T15:00:00,5Z", WantErr: true,
	}, { // Test 10: A period with no fraction digits.
		In: "2026-07-27T15:00:00.Z", WantErr: true,
	}, { // Test 11: A leap second.
		In: "2026-06-30T23:59:60Z", WantErr: true,
	}, { // Test 12: A day past the end of its month.
		In: "2026-02-29T00:00:00Z", WantErr: true,
	}, { // Test 13: Hour 24.
		In: "2026-07-27T24:00:00Z", WantErr: true,
	}, { // Test 14: Non-ASCII digits.
		In: "2026-07-27T15:00:0١Z", WantErr: true,
	}, { // Test 15: Empty.
		In: "", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseBundleTime(test.In)
			if (err != nil) != test.WantErr {
				t.Fatalf("parseBundleTime(%q) error = %v, want error %v", test.In, err, test.WantErr)
			}
			if diff := cmp.Diff(test.WantTime, got); diff != "" {
				t.Errorf("parseBundleTime(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
