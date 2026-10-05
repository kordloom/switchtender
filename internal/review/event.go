// Package review runs pull request review for a trigger: it reads GitHub pull_request and GitLab
// merge_request webhooks, and reports a plan-only run's result back to the pull request as one
// comment per template, updated in place, and a commit status. Applying stays in SwitchTender:
// after merge the apply goes through the normal gate and approval queue, and nothing here can
// release it.
package review

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/trigger"
)

// Event is one pull request or merge request webhook, reduced to what a review needs. Every field
// is read from a body whose signature or token was verified before it was parsed.
type Event struct {
	// Provider is github or gitlab.
	Provider string
	// Action is the forge's action name: opened, synchronize, reopened on GitHub, open, update,
	// reopen on GitLab.
	Action string
	// Plannable reports that the action proposes code that has not been planned yet: a pull
	// request opened or reopened, or new commits pushed to it. A title edit or a label is not.
	Plannable bool
	// Repository is the base repository the pull request targets: owner/name on GitHub, the
	// group/project path on GitLab.
	Repository string
	// Number is the pull request number on GitHub, the merge request iid on GitLab.
	Number int
	// HeadSHA is the full commit the pull request proposes, the commit the plan runs.
	HeadSHA string
	// HeadBranch is the branch the pull request comes from, for display.
	HeadBranch string
	// Fork reports that the head lives in another repository, which is how a person with no write
	// access proposes code. A head repository that has been deleted is reported as a fork.
	Fork bool
	// URL is the pull request's web address, for display.
	URL string
}

// shaPattern matches a full SHA-1 or SHA-256 commit id.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Ref returns the reference the base repository publishes the pull request's head under, which is
// fetchable even when the head lives in a fork.
func (e *Event) Ref() string {
	if e.Provider == trigger.ProviderGitLab {
		return "refs/merge-requests/" + strconv.Itoa(e.Number) + "/head"
	}
	return "refs/pull/" + strconv.Itoa(e.Number) + "/head"
}

// SameRepository reports whether the event's repository is repo, ignoring case and surrounding
// slashes, since both forges treat repository paths case-insensitively.
func (e *Event) SameRepository(repo string) bool {
	return strings.EqualFold(strings.Trim(e.Repository, "/"), strings.Trim(repo, "/"))
}

// githubRepo is the part of a GitHub repository object a review reads.
type githubRepo struct {
	// FullName is owner/name.
	FullName string `json:"full_name"`
}

// githubRef is one side of a GitHub pull request, its head or its base.
type githubRef struct {
	// SHA is the commit at this side.
	SHA string `json:"sha"`
	// Ref is the branch name.
	Ref string `json:"ref"`
	// Repo is the repository this side lives in, null for a deleted fork.
	Repo *githubRepo `json:"repo"`
}

// githubPayload is the part of a GitHub pull_request event a review reads.
type githubPayload struct {
	// Action is what happened to the pull request.
	Action string `json:"action"`
	// Number is the pull request number.
	Number int `json:"number"`
	// PullRequest is the pull request itself.
	PullRequest *struct {
		// HTMLURL is the pull request's web address.
		HTMLURL string `json:"html_url"`
		// Head is the side the code comes from.
		Head githubRef `json:"head"`
		// Base is the side the code is proposed into.
		Base githubRef `json:"base"`
	} `json:"pull_request"`
}

// githubPlanActions are the pull_request actions that propose code not yet planned.
var githubPlanActions = map[string]bool{
	"opened": true, "synchronize": true, "reopened": true, "ready_for_review": true,
}

// ParseGitHub reads a GitHub webhook. eventName is the X-GitHub-Event header. Anything but a
// pull_request event returns ErrNotReviewEvent.
func ParseGitHub(eventName string, body []byte) (*Event, error) {
	if eventName != "pull_request" {
		return nil, fmt.Errorf("%w: %q", ErrNotReviewEvent, eventName)
	}
	var p githubPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayload, err)
	}
	if p.PullRequest == nil || p.Number <= 0 {
		return nil, fmt.Errorf("%w: no pull request", ErrBadPayload)
	}
	pr := p.PullRequest
	if pr.Base.Repo == nil || pr.Base.Repo.FullName == "" {
		return nil, fmt.Errorf("%w: no base repository", ErrBadPayload)
	}
	sha := strings.ToLower(pr.Head.SHA)
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("%w: head sha %q", ErrBadPayload, pr.Head.SHA)
	}
	fork := pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName)
	return &Event{
		Provider: trigger.ProviderGitHub, Action: p.Action, Plannable: githubPlanActions[p.Action],
		Repository: pr.Base.Repo.FullName, Number: p.Number, HeadSHA: sha, HeadBranch: pr.Head.Ref,
		Fork: fork, URL: pr.HTMLURL,
	}, nil
}

// gitlabPayload is the part of a GitLab merge request event a review reads.
type gitlabPayload struct {
	// ObjectKind is merge_request for a merge request event.
	ObjectKind string `json:"object_kind"`
	// Project is the target project, the one the hook is configured on.
	Project struct {
		// PathWithNamespace is group/project.
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	// ObjectAttributes is the merge request itself.
	ObjectAttributes struct {
		// IID is the merge request number within its project.
		IID int `json:"iid"`
		// Action is what happened to the merge request.
		Action string `json:"action"`
		// OldRev is set on an update that pushed commits, and absent on one that only edited
		// metadata.
		OldRev string `json:"oldrev"`
		// SourceProjectID is where the code comes from.
		SourceProjectID int64 `json:"source_project_id"`
		// TargetProjectID is where the code is proposed into.
		TargetProjectID int64 `json:"target_project_id"`
		// SourceBranch is the branch the code comes from.
		SourceBranch string `json:"source_branch"`
		// URL is the merge request's web address.
		URL string `json:"url"`
		// LastCommit is the head of the source branch.
		LastCommit struct {
			// ID is the commit id.
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

// ParseGitLab reads a GitLab webhook. eventName is the X-Gitlab-Event header. Anything but a merge
// request event returns ErrNotReviewEvent.
func ParseGitLab(eventName string, body []byte) (*Event, error) {
	if eventName != "Merge Request Hook" {
		return nil, fmt.Errorf("%w: %q", ErrNotReviewEvent, eventName)
	}
	var p gitlabPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayload, err)
	}
	attrs := p.ObjectAttributes
	if p.ObjectKind != "merge_request" || attrs.IID <= 0 {
		return nil, fmt.Errorf("%w: no merge request", ErrBadPayload)
	}
	if p.Project.PathWithNamespace == "" {
		return nil, fmt.Errorf("%w: no project", ErrBadPayload)
	}
	sha := strings.ToLower(attrs.LastCommit.ID)
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("%w: head sha %q", ErrBadPayload, attrs.LastCommit.ID)
	}
	plannable := attrs.Action == "open" || attrs.Action == "reopen" ||
		(attrs.Action == "update" && attrs.OldRev != "")
	return &Event{
		Provider: trigger.ProviderGitLab, Action: attrs.Action, Plannable: plannable,
		Repository: p.Project.PathWithNamespace, Number: attrs.IID, HeadSHA: sha,
		HeadBranch: attrs.SourceBranch, Fork: attrs.SourceProjectID != attrs.TargetProjectID,
		URL: attrs.URL,
	}, nil
}

// Parse reads a webhook for provider using the event header that provider sends.
func Parse(provider, eventName string, body []byte) (*Event, error) {
	if provider == trigger.ProviderGitLab {
		return ParseGitLab(eventName, body)
	}
	return ParseGitHub(eventName, body)
}

// EventHeader returns the request header naming the event type for provider.
func EventHeader(provider string) string {
	if provider == trigger.ProviderGitLab {
		return "X-Gitlab-Event"
	}
	return "X-GitHub-Event"
}
