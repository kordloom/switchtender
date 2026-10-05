package review

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// headSHA is a well-formed commit id the payloads below propose.
const headSHA = "0123456789abcdef0123456789abcdef01234567"

// githubBody builds a GitHub pull_request payload. A head repository of "" is a deleted fork.
func githubBody(action, headRepo, sha string) []byte {
	head := `null`
	if headRepo != "" {
		head = `{"full_name":"` + headRepo + `"}`
	}
	return []byte(`{"action":"` + action + `","number":7,"pull_request":{` +
		`"html_url":"https://github.example.com/acme/infra/pull/7",` +
		`"head":{"sha":"` + sha + `","ref":"feature","repo":` + head + `},` +
		`"base":{"sha":"aaaa","ref":"main","repo":{"full_name":"acme/infra"}}}}`)
}

// gitlabBody builds a GitLab merge request payload.
func gitlabBody(action, oldrev string, source, target int, sha string) []byte {
	return []byte(fmt.Sprintf(`{"object_kind":"merge_request",`+
		`"project":{"path_with_namespace":"infra/network"},`+
		`"object_attributes":{"iid":12,"action":%q,"oldrev":%q,"source_project_id":%d,`+
		`"target_project_id":%d,"source_branch":"feature","url":"https://gitlab.example.com/mr/12",`+
		`"last_commit":{"id":%q}}}`, action, oldrev, source, target, sha))
}

// TestParseGitHub pins what a GitHub pull_request webhook plans, what it ignores, and how a fork is
// told apart.
func TestParseGitHub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Event         string
		Body          []byte
		WantPlannable bool
		WantFork      bool
		Want          error
	}{{ // Test 0: An opened pull request from the same repository is planned.
		Event: "pull_request", Body: githubBody("opened", "acme/infra", headSHA), WantPlannable: true,
	}, { // Test 1: A push to the pull request is planned.
		Event: "pull_request", Body: githubBody("synchronize", "acme/infra", headSHA),
		WantPlannable: true,
	}, { // Test 2: A label is not new code.
		Event: "pull_request", Body: githubBody("labeled", "acme/infra", headSHA),
	}, { // Test 3: A head in another repository is a fork.
		Event: "pull_request", Body: githubBody("opened", "outsider/infra", headSHA),
		WantPlannable: true, WantFork: true,
	}, { // Test 4: A deleted head repository is treated as a fork.
		Event: "pull_request", Body: githubBody("opened", "", headSHA),
		WantPlannable: true, WantFork: true,
	}, { // Test 5: A push event is not a review event.
		Event: "push", Body: []byte(`{}`), Want: ErrNotReviewEvent,
	}, { // Test 6: A head that is not a commit id is refused.
		Event: "pull_request", Body: githubBody("opened", "acme/infra", "main; rm -rf /"),
		Want: ErrBadPayload,
	}, { // Test 7: A body that is not JSON is refused.
		Event: "pull_request", Body: []byte(`not json`), Want: ErrBadPayload,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ev, err := ParseGitHub(test.Event, test.Body)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseGitHub() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if ev.Plannable != test.WantPlannable || ev.Fork != test.WantFork {
				t.Errorf("ParseGitHub() plannable %v fork %v, want %v and %v", ev.Plannable, ev.Fork,
					test.WantPlannable, test.WantFork)
			}
			if diff := cmp.Diff("refs/pull/7/head", ev.Ref()); diff != "" {
				t.Errorf("Ref() mismatch (-want +got):\n%s", diff)
			}
			if !ev.SameRepository("ACME/infra") || ev.SameRepository("acme/other") {
				t.Errorf("SameRepository() did not compare %q case-insensitively and exactly", ev.Repository)
			}
		})
	}
}

// TestParseGitLab pins what a GitLab merge request webhook plans: an update only when it pushed
// commits, and a fork by its source project.
func TestParseGitLab(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Event         string
		Body          []byte
		WantPlannable bool
		WantFork      bool
		Want          error
	}{{ // Test 0: An opened merge request is planned.
		Event: "Merge Request Hook", Body: gitlabBody("open", "", 5, 5, headSHA), WantPlannable: true,
	}, { // Test 1: An update that pushed commits is planned.
		Event: "Merge Request Hook", Body: gitlabBody("update", "ffff", 5, 5, headSHA),
		WantPlannable: true,
	}, { // Test 2: An update that only edited the description is not.
		Event: "Merge Request Hook", Body: gitlabBody("update", "", 5, 5, headSHA),
	}, { // Test 3: A source project other than the target is a fork.
		Event: "Merge Request Hook", Body: gitlabBody("open", "", 9, 5, headSHA),
		WantPlannable: true, WantFork: true,
	}, { // Test 4: A push hook is not a review event.
		Event: "Push Hook", Body: []byte(`{}`), Want: ErrNotReviewEvent,
	}, { // Test 5: A missing head commit is refused.
		Event: "Merge Request Hook", Body: gitlabBody("open", "", 5, 5, ""), Want: ErrBadPayload,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ev, err := ParseGitLab(test.Event, test.Body)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseGitLab() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if ev.Plannable != test.WantPlannable || ev.Fork != test.WantFork {
				t.Errorf("ParseGitLab() plannable %v fork %v, want %v and %v", ev.Plannable, ev.Fork,
					test.WantPlannable, test.WantFork)
			}
			if diff := cmp.Diff("refs/merge-requests/12/head", ev.Ref()); diff != "" {
				t.Errorf("Ref() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
