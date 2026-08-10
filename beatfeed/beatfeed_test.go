package beatfeed

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestBeatWireShape pins the beat's JSON exactly, so a renamed field or a changed tag fails here
// rather than silently splitting the server that produces the feed from the witness that reads it.
func TestBeatWireShape(t *testing.T) {
	t.Parallel()
	b := Beat{Beat: 7, At: "2026-08-10T11:50:00Z", Seq: 42, Head: "abc123"}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"beat":7,"at":"2026-08-10T11:50:00Z","seq":42,"head":"abc123"}`
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("beat wire shape drifted (-want +got):\n%s", diff)
	}

	// A feed answer round-trips back to the same beat, so a consumer decoding the contract recovers
	// exactly what a producer encoded.
	var back Beat
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(b, back); diff != "" {
		t.Errorf("beat did not round-trip (-want +got):\n%s", diff)
	}
}

// TestFeedPathContract pins the path constants, since the witness builds its request URL from them
// and the server's auth carve-out leaves exactly this path open.
func TestFeedPathContract(t *testing.T) {
	t.Parallel()
	if FeedPath != "/audit/beats" {
		t.Errorf("FeedPath = %q, want /audit/beats", FeedPath)
	}
	if APIPath != "/v1/audit/beats" {
		t.Errorf("APIPath = %q, want /v1/audit/beats", APIPath)
	}
	if LimitParam != "limit" {
		t.Errorf("LimitParam = %q, want limit", LimitParam)
	}
}
