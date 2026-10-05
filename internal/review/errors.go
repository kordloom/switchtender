package review

import "errors"

var (
	// ErrNotReviewEvent is returned for a webhook that is not a pull request or merge request
	// event, such as a push, a ping, or an issue comment. It is not a failure: the hook answers it
	// as ignored.
	ErrNotReviewEvent = errors.New("not a pull request event")
	// ErrBadPayload is returned when a pull request event cannot be read or lacks a field a plan
	// needs, such as the head commit.
	ErrBadPayload = errors.New("malformed pull request event")
	// ErrForge is returned when the forge's API refuses or fails a request the review makes.
	ErrForge = errors.New("forge api request")
	// ErrNoToken is returned when the review's token credential cannot be opened.
	ErrNoToken = errors.New("review token unavailable")
	// ErrNotCommentEvent is returned for a webhook that is not a newly created comment on a pull
	// request or merge request, such as an edit, a deletion, or a comment on an issue. It is not a
	// failure: the hook answers it as ignored.
	ErrNotCommentEvent = errors.New("not a new pull request comment")
	// ErrRecordNotFound is returned when no report record exists for an id.
	ErrRecordNotFound = errors.New("review report record not found")
)
