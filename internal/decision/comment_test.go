package decision

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCloneCopiesComment pins that a clone carries the comment a decision was made from as its own
// copy, so a store handing out clones never lets a caller rewrite the stored evidence through one.
func TestCloneCopiesComment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In *Record
	}{{ // Test 0: A decision made from a comment.
		In: &Record{ID: "aud_c", Kind: KindDecision, Comment: &Comment{Forge: "gitlab",
			APIURL: "https://gitlab.com/api/v4", Repository: "platform/infra", PullRequest: 7,
			CommentID: 99, AuthorID: 12, BodySHA256: "ab"}},
	}, { // Test 1: A decision made any other way.
		In: &Record{ID: "aud_p", Kind: KindDecision},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := test.In.Clone()
			if diff := cmp.Diff(test.In, got); diff != "" {
				t.Errorf("Clone() mismatch (-want +got):\n%s", diff)
			}
			if test.In.Comment == nil {
				return
			}
			if got.Comment == test.In.Comment {
				t.Errorf("Clone() shares the comment with the original")
			}
			got.Comment.CommentID = 1
			if test.In.Comment.CommentID != 99 {
				t.Errorf("changing the clone's comment changed the original's to %d",
					test.In.Comment.CommentID)
			}
		})
	}
}
