package run

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
)

func TestSummaryName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 5000)
	longSum := sha256.Sum256([]byte(long))
	runes := strings.Repeat("é", 3000)
	tests := []struct {
		In          string
		WantName    string
		WantCut     bool
		WantMaxSize int
	}{{ // Test 0: An ordinary host name is kept exactly.
		In: "web01.example.com", WantName: "web01.example.com",
	}, { // Test 1: An empty name stays empty.
		In: "", WantName: "",
	}, { // Test 2: A name exactly at the limit is kept exactly.
		In: strings.Repeat("b", MaxSummaryNameBytes), WantName: strings.Repeat("b", MaxSummaryNameBytes),
	}, { // Test 3: A multi-byte name at the limit is kept exactly.
		In:       strings.Repeat("é", MaxSummaryNameBytes/2),
		WantName: strings.Repeat("é", MaxSummaryNameBytes/2),
	}, { // Test 4: A name with a byte a text column cannot hold is made safe and otherwise kept.
		In: "web\x0001", WantName: "web�01",
	}, { // Test 5: A long name keeps its start and ends in the digest of the whole name.
		In: long, WantCut: true, WantMaxSize: MaxSummaryNameBytes,
		WantName: long[:MaxSummaryNameBytes-len(summaryDigestMark)-sha256.Size*2] +
			summaryDigestMark + hex.EncodeToString(longSum[:]),
	}, { // Test 6: A long multi-byte name is cut on a rune boundary.
		In: runes, WantCut: true, WantMaxSize: MaxSummaryNameBytes,
	}, { // Test 7: One byte over the limit is cut.
		In: strings.Repeat("c", MaxSummaryNameBytes+1), WantCut: true,
		WantMaxSize: MaxSummaryNameBytes,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := SummaryName(test.In)
			if test.WantName != "" || !test.WantCut {
				if diff := cmp.Diff(test.WantName, got); diff != "" {
					t.Errorf("SummaryName() mismatch (-want +got):\n%s", diff)
				}
			}
			if !test.WantCut {
				return
			}
			if len(got) > test.WantMaxSize || !utf8.ValidString(got) {
				t.Errorf("SummaryName() = %d bytes, valid UTF-8 %v, want at most %d and valid",
					len(got), utf8.ValidString(got), test.WantMaxSize)
			}
			sum := sha256.Sum256([]byte(test.In))
			if !strings.HasSuffix(got, summaryDigestMark+hex.EncodeToString(sum[:])) {
				t.Errorf("SummaryName() does not end in the digest of the whole name")
			}
			if again := SummaryName(got); again != got {
				t.Errorf("SummaryName() of a cut name changed it again")
			}
		})
	}
}

func TestSummaryNameKeys(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 2*MaxSummaryNameBytes)
	tests := []struct {
		In         string
		WantWhole  string
		WantStored string
	}{{ // Test 0: An ordinary name is matched as itself twice.
		In: "db01", WantWhole: "db01", WantStored: "db01",
	}, { // Test 1: A long name is matched whole, as older rows hold it, and cut, as newer rows do.
		In: long, WantWhole: long, WantStored: SummaryName(long),
	}, { // Test 2: A name with an unrepresentable byte is matched as it was stored, made safe.
		In: "db\xff", WantWhole: "db�", WantStored: "db�",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			whole, stored := SummaryNameKeys(test.In)
			if diff := cmp.Diff(test.WantWhole, whole); diff != "" {
				t.Errorf("SummaryNameKeys() whole mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantStored, stored); diff != "" {
				t.Errorf("SummaryNameKeys() stored mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
