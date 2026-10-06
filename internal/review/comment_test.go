package review

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/trigger"
)

// githubCommentBody builds a GitHub issue_comment payload. onPR puts the comment on a pull request
// rather than an issue.
func githubCommentBody(action string, onPR bool, kind string) []byte {
	pr := ``
	if onPR {
		pr = `,"pull_request":{"url":"https://github.example.com/pulls/7"}`
	}
	return []byte(`{"action":"` + action + `","issue":{"number":7` + pr + `},` +
		`"comment":{"id":901,"body":"/switchtender apply","user":{"id":1001,"login":"forge-person",` +
		`"type":"` + kind + `"}},"repository":{"full_name":"acme/infra"}}`)
}

// gitlabTargetKey is the field naming what a GitLab note is on, spelled as GitLab spells it.
const gitlabTargetKey = "noteable_type" //nolint:misspell // GitLab's own field name.

// gitlabNoteBody builds a GitLab note payload on a target of the given type.
func gitlabNoteBody(target, action string, system bool) []byte {
	return []byte(fmt.Sprintf(`{"object_kind":"note","user":{"id":1001},`+
		`"project":{"path_with_namespace":"infra/network"},"object_attributes":{"id":902,`+
		`"note":"/switchtender plan",%q:%q,"author_id":1001,"system":%v,"action":%q},`+
		`"merge_request":{"iid":12}}`, gitlabTargetKey, target, system, action))
}

// TestParseComment pins which comment webhooks a command reads: a new comment on a pull request, by
// its numeric ids, and never an edit, a deletion, an issue comment, or a system note.
func TestParseComment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider  string
		Event     string
		Body      []byte
		WantEvent *CommentEvent
		Want      error
	}{{ // Test 0: A new GitHub pull request comment is read by its numeric ids.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: githubCommentBody("created", true, "User"),
		WantEvent: &CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra",
			Number: 7, CommentID: 901, AuthorID: 1001, Body: "/switchtender apply"},
	}, { // Test 1: A GitHub bot is marked as one.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: githubCommentBody("created", true, "Bot"),
		WantEvent: &CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra",
			Number: 7, CommentID: 901, AuthorID: 1001, AuthorBot: true, Body: "/switchtender apply"},
	}, { // Test 2: An edited comment never acts.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: githubCommentBody("edited", true, "User"), Want: ErrNotCommentEvent,
	}, { // Test 3: A deleted comment never acts.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: githubCommentBody("deleted", true, "User"), Want: ErrNotCommentEvent,
	}, { // Test 4: A comment on an issue is not a pull request comment.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: githubCommentBody("created", false, "User"), Want: ErrNotCommentEvent,
	}, { // Test 5: Another GitHub event is not a comment.
		Provider: trigger.ProviderGitHub, Event: "push", Body: []byte(`{}`),
		Want: ErrNotCommentEvent,
	}, { // Test 6: A GitHub comment missing its author is refused.
		Provider: trigger.ProviderGitHub, Event: "issue_comment",
		Body: []byte(`{"action":"created","issue":{"number":7,"pull_request":{}},` +
			`"comment":{"id":901,"body":"x","user":{}},"repository":{"full_name":"acme/infra"}}`),
		Want: ErrBadPayload,
	}, { // Test 7: A new GitLab merge request note is read by its numeric ids.
		Provider: trigger.ProviderGitLab, Event: "Note Hook",
		Body: gitlabNoteBody("MergeRequest", "create", false),
		WantEvent: &CommentEvent{Provider: trigger.ProviderGitLab, Repository: "infra/network",
			Number: 12, CommentID: 902, AuthorID: 1001, Body: "/switchtender plan"},
	}, { // Test 8: A GitLab release that sends no action is a new note.
		Provider: trigger.ProviderGitLab, Event: "Note Hook",
		Body: gitlabNoteBody("MergeRequest", "", false),
		WantEvent: &CommentEvent{Provider: trigger.ProviderGitLab, Repository: "infra/network",
			Number: 12, CommentID: 902, AuthorID: 1001, Body: "/switchtender plan"},
	}, { // Test 9: An updated GitLab note never acts.
		Provider: trigger.ProviderGitLab, Event: "Note Hook",
		Body: gitlabNoteBody("MergeRequest", "update", false), Want: ErrNotCommentEvent,
	}, { // Test 10: A system note never acts.
		Provider: trigger.ProviderGitLab, Event: "Note Hook",
		Body: gitlabNoteBody("MergeRequest", "create", true), Want: ErrNotCommentEvent,
	}, { // Test 11: A note on an issue is not a merge request comment.
		Provider: trigger.ProviderGitLab, Event: "Note Hook",
		Body: gitlabNoteBody("Issue", "create", false), Want: ErrNotCommentEvent,
	}, { // Test 12: A merge request event is not a comment.
		Provider: trigger.ProviderGitLab, Event: "Merge Request Hook", Body: []byte(`{}`),
		Want: ErrNotCommentEvent,
	}, { // Test 13: A note that does not decode is refused.
		Provider: trigger.ProviderGitLab, Event: "Note Hook", Body: []byte(`{`),
		Want: ErrBadPayload,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ParseComment(test.Provider, test.Event, test.Body)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseComment() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantEvent, got); diff != "" {
				t.Errorf("ParseComment() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCommentRecordsShareTheCommentKey proves a comment's command and reply records are keyed by
// the comment itself, so a redelivery finds them, and that another comment, repository, or forge
// gets its own.
func TestCommentRecordsShareTheCommentKey(t *testing.T) {
	t.Parallel()
	tg := &trigger.Trigger{ID: "trg_1", Review: &trigger.Review{Provider: trigger.ProviderGitHub,
		Repository: "acme/infra"}}
	other := &trigger.Trigger{ID: "trg_1", Review: &trigger.Review{Provider: trigger.ProviderGitHub,
		APIURL: "https://github.example.com/api/v3", Repository: "acme/infra"}}
	ev := &CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra", Number: 7,
		CommentID: 901, AuthorID: 1001, Body: "/switchtender apply"}
	at := time.Unix(1700000000, 0)
	tests := []struct {
		Trigger  *trigger.Trigger
		Event    *CommentEvent
		WantSame bool
	}{{ // Test 0: The same comment delivered again has the same key.
		Trigger: tg, Event: &CommentEvent{Provider: trigger.ProviderGitHub,
			Repository: "ACME/infra", Number: 7, CommentID: 901, AuthorID: 1001, Body: "edited"},
		WantSame: true,
	}, { // Test 1: Another comment has its own.
		Trigger: tg, Event: &CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra",
			Number: 7, CommentID: 902, AuthorID: 1001},
	}, { // Test 2: The same comment id on another forge has its own.
		Trigger: other, Event: ev,
	}, { // Test 3: The same comment id in another repository has its own.
		Trigger: tg, Event: &CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/other",
			Number: 7, CommentID: 901, AuthorID: 1001},
	}, { // Test 4: The same comment reaching another trigger on the repository has its own.
		Trigger: &trigger.Trigger{ID: "trg_2", Review: tg.Review}, Event: ev,
	}}
	base := CommandRecord(tg, ev, at)
	if !base.Done || base.Kind != KindCommand || base.Reason != CommandApply {
		t.Fatalf("CommandRecord() = %+v, want a done command record naming apply", base)
	}
	reply := ReplyRecord(tg, ev, "linked?", at)
	if reply.Done || reply.Kind != KindReply || reply.Reason != "linked?" {
		t.Fatalf("ReplyRecord() = %+v, want an owed reply", reply)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := CommandRecord(test.Trigger, test.Event, at)
			if same := got.ID == base.ID; same != test.WantSame {
				t.Errorf("CommandRecord().ID = %s against %s, same %v, want %v", got.ID, base.ID,
					same, test.WantSame)
			}
			r := ReplyRecord(test.Trigger, test.Event, "x", at)
			if diff := cmp.Diff(got.ID[len("cmd_"):], r.ID[len("rpl_"):],
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reply and command keys differ (-command +reply):\n%s", diff)
			}
		})
	}
}
