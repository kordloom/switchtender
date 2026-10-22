package audit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestReleasePathRoundTrip pins the release path both ways: what ReleasePath writes,
// ParseReleasePath reads back exactly, and every other path, a step decision's among them, is
// refused.
func TestReleasePathRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path       string
		WantParent string
		WantChild  string
		WantOK     bool
	}{{ // Test 0: The path ReleasePath writes.
		Path: ReleasePath("run_wf", "run_step"), WantParent: "run_wf", WantChild: "run_step",
		WantOK: true,
	}, { // Test 1: The literal form, so the encoding is pinned rather than only self-consistent.
		Path: "/runs/run_wf/steps/run_step/release", WantParent: "run_wf", WantChild: "run_step",
		WantOK: true,
	}, { // Test 2: A step decision's path is not a release.
		Path: "/runs/run_wf/steps/run_step/decision/approved",
	}, { // Test 3: A run's outcome path is not a release.
		Path: "/runs/run_wf/outcome/succeeded",
	}, { // Test 4: An empty workflow id.
		Path: "/runs//steps/run_step/release",
	}, { // Test 5: An empty step id.
		Path: "/runs/run_wf/steps//release",
	}, { // Test 6: A trailing slash adds a segment.
		Path: "/runs/run_wf/steps/run_step/release/",
	}, { // Test 7: A step id holding a slash does not round-trip.
		Path: ReleasePath("run_wf", "run/step"),
	}, { // Test 8: Another word in place of steps.
		Path: "/runs/run_wf/shards/run_step/release",
	}, { // Test 9: Another word in place of release.
		Path: "/runs/run_wf/steps/run_step/released",
	}, { // Test 10: A path outside /runs.
		Path: "/v1/runs/run_wf/steps/run_step/release",
	}, { // Test 11: The empty path.
		Path: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			parent, child, ok := ParseReleasePath(test.Path)
			if ok != test.WantOK {
				t.Fatalf("ParseReleasePath(%q) ok = %v, want %v", test.Path, ok, test.WantOK)
			}
			if diff := cmp.Diff([]string{test.WantParent, test.WantChild},
				[]string{parent, child}); diff != "" {
				t.Errorf("ids mismatch (-want +got):\n%s", diff)
			}
			if ok && ReleasePath(parent, child) != test.Path {
				t.Errorf("ReleasePath(%q, %q) = %q, want %q", parent, child,
					ReleasePath(parent, child), test.Path)
			}
		})
	}
}

// TestReleaseIDIsFixedByItsInputs pins the release id to the formula the chain depends on: "aud_"
// and 32 hex digits of the SHA-256 over the sorted root ids, the parent, the index, and the
// attempt, joined by NUL bytes. Two launchers of one attempt must compute one id, and any change
// to what released the attempt must compute another.
func TestReleaseIDIsFixedByItsInputs(t *testing.T) {
	t.Parallel()
	base := ReleaseID([]string{"aud_aaa", "aud_bbb"}, "run_wf", 2, 3)
	tests := []struct {
		Roots    []string
		Parent   string
		WantID   string
		Index    int
		Attempt  int
		WantSame bool
	}{{ // Test 0: The pinned vector, computed outside Go with printf and shasum.
		Roots: []string{"aud_bbb", "aud_aaa"}, Parent: "run_wf", Index: 2, Attempt: 3,
		WantID: "aud_aedca4aeafb17873ef68c00db3261f70", WantSame: true,
	}, { // Test 1: No roots still names one fixed entry.
		Parent: "run_wf", Index: 0, Attempt: 1, WantID: "aud_680f410bbd7a14b41ea656ddd64950ad",
	}, { // Test 2: The roots in either order are the same release.
		Roots: []string{"aud_aaa", "aud_bbb"}, Parent: "run_wf", Index: 2, Attempt: 3,
		WantSame: true,
	}, { // Test 3: Another attempt is another release.
		Roots: []string{"aud_aaa", "aud_bbb"}, Parent: "run_wf", Index: 2, Attempt: 4,
	}, { // Test 4: Another step is another release.
		Roots: []string{"aud_aaa", "aud_bbb"}, Parent: "run_wf", Index: 1, Attempt: 3,
	}, { // Test 5: Another workflow is another release.
		Roots: []string{"aud_aaa", "aud_bbb"}, Parent: "run_other", Index: 2, Attempt: 3,
	}, { // Test 6: A missing root is another release.
		Roots: []string{"aud_aaa"}, Parent: "run_wf", Index: 2, Attempt: 3,
	}, { // Test 7: An extra root is another release.
		Roots: []string{"aud_aaa", "aud_bbb", "aud_ccc"}, Parent: "run_wf", Index: 2, Attempt: 3,
	}, { // Test 8: A substituted root is another release.
		Roots: []string{"aud_aaa", "aud_bbc"}, Parent: "run_wf", Index: 2, Attempt: 3,
	}, { // Test 9: Two roots run together are not one root, since NUL separates them.
		Roots: []string{"aud_aaaaud_bbb"}, Parent: "run_wf", Index: 2, Attempt: 3,
	}, { // Test 10: Index 2 attempt 3 is not index 23 attempt 0 or anything else run together.
		Roots: []string{"aud_aaa", "aud_bbb"}, Parent: "run_wf", Index: 23, Attempt: 0,
	}}
	shape := regexp.MustCompile(`^aud_[0-9a-f]{32}$`)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			roots := append([]string(nil), test.Roots...)
			got := ReleaseID(roots, test.Parent, test.Index, test.Attempt)
			if !shape.MatchString(got) {
				t.Errorf("ReleaseID() = %q, want aud_ and 32 lowercase hex digits", got)
			}
			if test.WantID != "" && got != test.WantID {
				t.Errorf("ReleaseID() = %q, want %q", got, test.WantID)
			}
			if again := ReleaseID(roots, test.Parent, test.Index, test.Attempt); again != got {
				t.Errorf("ReleaseID() = %q then %q for the same inputs", got, again)
			}
			if (got == base) != test.WantSame {
				t.Errorf("ReleaseID() = %q beside %q, want same = %v", got, base, test.WantSame)
			}
			if diff := cmp.Diff(test.Roots, roots, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ReleaseID() reordered its input (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStartIDIsFixedByItsInputs pins the start id to the formula the chain depends on: "aud_" and
// 32 hex digits of the SHA-256 over the run id and the attempt joined by a NUL byte. Every start of
// one attempt must compute one id, and another run or attempt must compute another.
func TestStartIDIsFixedByItsInputs(t *testing.T) {
	t.Parallel()
	base := StartID("run_x", 1)
	tests := []struct {
		RunID    string
		WantID   string
		WantPath string
		Attempt  int
		WantSame bool
	}{{ // Test 0: The pinned vector, computed outside Go with printf and shasum.
		RunID: "run_x", Attempt: 1, WantID: "aud_84394059cebcd1e8515bfc827b29b3a4",
		WantPath: "/runs/run_x/start/1", WantSame: true,
	}, { // Test 1: Another attempt is another start.
		RunID: "run_x", Attempt: 0, WantID: "aud_31defbc9e359ab94299378fea6715e23",
		WantPath: "/runs/run_x/start/0",
	}, { // Test 2: The run id and attempt run together are not the same start, since NUL separates them.
		RunID: "run_x1", Attempt: 0, WantPath: "/runs/run_x1/start/0",
	}, { // Test 3: Another run is another start.
		RunID: "run_y", Attempt: 1, WantPath: "/runs/run_y/start/1",
	}}
	shape := regexp.MustCompile(`^aud_[0-9a-f]{32}$`)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := StartID(test.RunID, test.Attempt)
			if !shape.MatchString(got) {
				t.Errorf("StartID() = %q, want aud_ and 32 lowercase hex digits", got)
			}
			if test.WantID != "" && got != test.WantID {
				t.Errorf("StartID() = %q, want %q", got, test.WantID)
			}
			if again := StartID(test.RunID, test.Attempt); again != got {
				t.Errorf("StartID() = %q then %q for the same inputs", got, again)
			}
			if (got == base) != test.WantSame {
				t.Errorf("StartID() = %q beside %q, want same = %v", got, base, test.WantSame)
			}
			if path := StartPath(test.RunID, test.Attempt); path != test.WantPath {
				t.Errorf("StartPath() = %q, want %q", path, test.WantPath)
			}
			payload := map[string]any{"method": MethodStart, "path": StartPath(test.RunID, test.Attempt)}
			if kind := recordKindOf(payload); kind != "" {
				t.Errorf("recordKindOf(START) = %q, want a plain claim", kind)
			}
		})
	}
}

// TestASecondReleaseOfOneAttemptIsRefused pins what the fixed id is for: the chain takes the
// release of an attempt once, and a second launcher of that attempt learns it is already there
// rather than recording a second release.
func TestASecondReleaseOfOneAttemptIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	id := ReleaseID([]string{"aud_aaa"}, "run_wf", 1, 1)
	entry := func() *Entry {
		return &Entry{ID: id, At: time.Now(), Actor: "system:dispatcher", ActorType: "system",
			Method: MethodRelease, Path: ReleasePath("run_wf", "run_step")}
	}
	if err := store.Append(ctx, entry()); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	if err := store.Append(ctx, entry()); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("second Append() error = %v, want %v", err, ErrDuplicateID)
	}
	got, err := store.Entry(ctx, id)
	if err != nil {
		t.Fatalf("Entry() error = %v", err)
	}
	if got.Method != MethodRelease || got.Path != ReleasePath("run_wf", "run_step") {
		t.Errorf("Entry() = %s %s, want the release", got.Method, got.Path)
	}
}
