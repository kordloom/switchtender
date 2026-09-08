package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// stubApprover records the decision it was asked for and returns canned results, so the approval
// handlers' error mapping can be driven without a dispatcher.
type stubApprover struct {
	// result is returned on success.
	result *run.Run
	// err is returned instead of result when non-nil.
	err error
	// gotID, gotBy, gotByType, and gotReason record the most recent decision.
	gotID     string
	gotBy     string
	gotByType string
	gotReason string
	// called counts decisions, so a test can prove a refusal happened before the dispatcher.
	called int
}

// Approve records the arguments and returns the configured run or error.
func (s *stubApprover) Approve(_ context.Context, id, by, byType string) (*run.Run, error) {
	s.called++
	s.gotID, s.gotBy, s.gotByType = id, by, byType
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// Reject records the arguments and returns the configured run or error.
func (s *stubApprover) Reject(_ context.Context, id, reason, by, byType string) (*run.Run, error) {
	s.called++
	s.gotID, s.gotReason, s.gotBy, s.gotByType = id, reason, by, byType
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// heldRun returns a run waiting on approval, optionally one whose rule demands a second person.
func heldRun(t *testing.T, distinct bool) *run.Run {
	t.Helper()
	return &run.Run{
		ID: "run_held", Playbook: "prod.yml", Status: run.StatusPendingApproval,
		Actor: "casey-token", ActorUserID: "user_1", RequireDistinctApprover: distinct,
		CreatedAt: time.Now(),
	}
}

// TestApprovalDecisionErrorMapping pins how every failure a decision can meet is reported. These
// two endpoints are the ones that release a held run onto real hosts, so a refusal reported as a
// success, or a conflict reported as a server error, is the difference between an operator finding a
// second approver and an operator retrying until something gives.
//
//nolint:funlen // Test function.
func TestApprovalDecisionErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Decision    string
		ApproverErr error
		NoApprover  bool
		NoRun       bool
		StoreErr    error
		WantStatus  int
		WantDecided bool
	}{{ // Test 0: Approvals are not wired, so the endpoint says so rather than pretending.
		Name: "approve without an approver", Decision: "approve", NoApprover: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 1: The same for a rejection.
		Name: "reject without an approver", Decision: "reject", NoApprover: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 2: A run that does not exist is a not found, decided before the dispatcher is asked.
		Name: "approve a missing run", Decision: "approve", NoRun: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 3: The same for a rejection.
		Name: "reject a missing run", Decision: "reject", NoRun: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 4: A store that cannot be read is a server error, and no decision is taken.
		Name: "approve with an unreadable store", Decision: "approve", StoreErr: errStore,
		WantStatus: http.StatusInternalServerError,
	}, { // Test 5: The same for a rejection.
		Name: "reject with an unreadable store", Decision: "reject", StoreErr: errStore,
		WantStatus: http.StatusInternalServerError,
	}, { // Test 6: A run that is not waiting on anybody is a conflict, so approving twice cannot
		// launch it twice.
		Name: "approve a run that is not held", Decision: "approve",
		ApproverErr: dispatch.ErrNotPendingApproval, WantStatus: http.StatusConflict,
		WantDecided: true,
	}, { // Test 7: The same for a rejection.
		Name: "reject a run that is not held", Decision: "reject",
		ApproverErr: dispatch.ErrNotPendingApproval, WantStatus: http.StatusConflict,
		WantDecided: true,
	}, { // Test 8: A shard or a step is decided through its parent, so deciding it alone is a
		// conflict rather than a partial release of one piece of a run.
		Name: "approve a child", Decision: "approve",
		ApproverErr: dispatch.ErrChildNotApprovable, WantStatus: http.StatusConflict,
		WantDecided: true,
	}, { // Test 9: The same for a rejection.
		Name: "reject a child", Decision: "reject",
		ApproverErr: dispatch.ErrChildNotApprovable, WantStatus: http.StatusConflict,
		WantDecided: true,
	}, { // Test 10: The dispatcher's own separation-of-duties refusal is a conflict, and it is the
		// one error whose message is passed through so the caller reads the rule's words.
		Name: "approve refused for self approval", Decision: "approve",
		ApproverErr: dispatch.ErrSelfApproval, WantStatus: http.StatusConflict, WantDecided: true,
	}, { // Test 11: Anything else is a server error rather than a conflict, so a real fault is not
		// dressed up as a rule.
		Name: "approve with an unknown failure", Decision: "approve", ApproverErr: errStore,
		WantStatus: http.StatusInternalServerError, WantDecided: true,
	}, { // Test 12: The same for a rejection.
		Name: "reject with an unknown failure", Decision: "reject", ApproverErr: errStore,
		WantStatus: http.StatusInternalServerError, WantDecided: true,
	}, { // Test 13: A run the dispatcher reports as gone between the read and the decision is still
		// a not found rather than a server error.
		Name: "approve a run that vanished", Decision: "approve", ApproverErr: run.ErrNotFound,
		WantStatus: http.StatusNotFound, WantDecided: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := &stubRuns{}
			if !test.NoRun && test.StoreErr == nil {
				store.present = heldRun(t, false)
			}
			store.getErr = test.StoreErr
			approver := &stubApprover{result: heldRun(t, false), err: test.ApproverErr}
			var handler http.HandlerFunc
			var a Approver = approver
			if test.NoApprover {
				a = nil
			}
			if test.Decision == "approve" {
				handler = approveRunHandler(a, store, nil, zap.NewNop())
			} else {
				handler = rejectRunHandler(a, store, nil, zap.NewNop())
			}
			req := httptest.NewRequest(http.MethodPost,
				"/v1/runs/run_held/"+test.Decision, strings.NewReader(""))
			req.SetPathValue("id", "run_held")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			decided := approver.called > 0
			if decided != test.WantDecided {
				t.Errorf("%s: dispatcher asked = %v, want %v", test.Name, decided, test.WantDecided)
			}
		})
	}
}

// stubRuns is a run.Store that answers Get from a fixed run, leaving every other method to the
// memory store underneath so a handler that reaches for one is not answering against nil.
type stubRuns struct {
	// Store is the memory store the unused methods fall through to.
	run.Store
	// present is what Get answers with, nil to answer run.ErrNotFound.
	present *run.Run
	// getErr replaces the ordinary answer of Get.
	getErr error
}

// Get answers with the fixed run, its configured error, or run.ErrNotFound.
func (s *stubRuns) Get(context.Context, string) (*run.Run, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, run.ErrNotFound
	}
	return s.present, nil
}

// TestSelfApprovalIsRefusedByAccountNotByLabel pins separation of duties where it is actually
// decidable. The actor recorded on a run is the credential's name, so a person who submitted with
// their API token and approves in their browser looks like two different names, and a comparison of
// names alone lets the requester sign off on their own change. The account is what identifies the
// person, so it is compared first.
//
// Rejecting your own run stays allowed throughout: withdrawing a request needs nobody else, and
// blocking it would leave a requester unable to take back their own change.
//
//nolint:funlen // Test function.
func TestSelfApprovalIsRefusedByAccountNotByLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Decision    string
		Distinct    bool
		Actor       Actor
		HasActor    bool
		WantStatus  int
		WantDecided bool
	}{{ // Test 0: The requester approving their own held run through a different credential is
		// refused, because the account matches even though the label does not.
		Name: "same account different credential", Decision: "approve", Distinct: true,
		Actor:    Actor{UserID: "user_1", Name: "casey-browser-session", Role: user.RoleAdmin},
		HasActor: true, WantStatus: http.StatusConflict, WantDecided: false,
	}, { // Test 1: A second person approving the same run is allowed.
		Name: "a different account", Decision: "approve", Distinct: true,
		Actor:    Actor{UserID: "user_2", Name: "drew", Role: user.RoleAdmin},
		HasActor: true, WantStatus: http.StatusOK, WantDecided: true,
	}, { // Test 2: Without the distinct-approver rule the requester may approve their own run, since
		// no rule said otherwise.
		Name: "no distinct rule", Decision: "approve", Distinct: false,
		Actor:    Actor{UserID: "user_1", Name: "casey-browser-session", Role: user.RoleAdmin},
		HasActor: true, WantStatus: http.StatusOK, WantDecided: true,
	}, { // Test 3: The requester may always reject their own run, which is how a request is
		// withdrawn.
		Name: "requester rejects their own", Decision: "reject", Distinct: true,
		Actor:    Actor{UserID: "user_1", Name: "casey-browser-session", Role: user.RoleAdmin},
		HasActor: true, WantStatus: http.StatusOK, WantDecided: true,
	}, { // Test 4: An install running without authentication has no actor to compare, so the check
		// does not fire and the dispatcher's own guard is what remains.
		Name: "open install", Decision: "approve", Distinct: true, HasActor: false,
		WantStatus: http.StatusOK, WantDecided: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			held := heldRun(t, test.Distinct)
			store := &stubRuns{present: held}
			approver := &stubApprover{result: held}
			var handler http.HandlerFunc
			if test.Decision == "approve" {
				handler = approveRunHandler(approver, store, nil, zap.NewNop())
			} else {
				handler = rejectRunHandler(approver, store, nil, zap.NewNop())
			}
			req := httptest.NewRequest(http.MethodPost,
				"/v1/runs/run_held/"+test.Decision, strings.NewReader(""))
			req.SetPathValue("id", "run_held")
			if test.HasActor {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			if decided := approver.called > 0; decided != test.WantDecided {
				t.Errorf("%s: dispatcher asked = %v, want %v",
					test.Name, decided, test.WantDecided)
			}
			if test.WantStatus == http.StatusConflict &&
				!strings.Contains(rec.Body.String(), "reject it to withdraw") {
				t.Errorf("%s: the refusal does not tell the requester what they can still do: %q",
					test.Name, rec.Body.String())
			}
		})
	}
}

// TestRejectionBodyIsHeldToTheStrictRule pins that a rejection's optional reason is optional but not
// lenient. An absent body is a rejection with no stated cause, which is allowed, while a body that
// is present is refused on a misspelled field so the trail never records a rejection whose stated
// cause quietly went missing.
func TestRejectionBodyIsHeldToTheStrictRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Body       string
		WantStatus int
		WantReason string
	}{{ // Test 0: No body at all is a rejection with no reason.
		Name: "absent body", Body: "", WantStatus: http.StatusOK,
	}, { // Test 1: An empty JSON object is the same.
		Name: "empty object", Body: `{}`, WantStatus: http.StatusOK,
	}, { // Test 2: A stated reason is carried through to the decision.
		Name: "with a reason", Body: `{"reason":"changes production DNS"}`,
		WantStatus: http.StatusOK, WantReason: "changes production DNS",
	}, { // Test 3: A misspelled reason is refused rather than dropped, so the trail cannot record a
		// rejection whose cause silently vanished.
		Name: "misspelled reason", Body: `{"resaon":"changes production DNS"}`,
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: A malformed body is refused.
		Name: "malformed body", Body: `{"reason":`, WantStatus: http.StatusBadRequest,
	}, { // Test 5: A reason of the wrong type is refused.
		Name: "wrong type", Body: `{"reason":42}`, WantStatus: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			held := heldRun(t, false)
			approver := &stubApprover{result: held}
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/reject",
				strings.NewReader(test.Body))
			req.SetPathValue("id", "run_held")
			rec := httptest.NewRecorder()
			rejectRunHandler(approver, &stubRuns{present: held}, nil, zap.NewNop()).
				ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantStatus != http.StatusOK {
				if approver.called != 0 {
					t.Errorf("%s: a refused body still reached the dispatcher", test.Name)
				}
				return
			}
			if approver.gotReason != test.WantReason {
				t.Errorf("%s: reason = %q, want %q", test.Name, approver.gotReason, test.WantReason)
			}
		})
	}
}

// TestApprovalRecordsWhoDecidedAndHow pins that the decision carries the caller's audit name and how
// they authenticated. The chain entry for a release names the approver, and an entry that cannot say
// whether a person or an agent signed off is the one thing the identity stage of this boundary
// exists to prevent.
func TestApprovalRecordsWhoDecidedAndHow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Actor      Actor
		HasActor   bool
		WantBy     string
		WantByType string
	}{{ // Test 0: A person at a browser is recorded as a session.
		Name: "session", Actor: Actor{UserID: "user_2", Name: "drew", Type: actorTypeSession},
		HasActor: true, WantBy: "drew", WantByType: actorTypeSession,
	}, { // Test 1: A script's token is recorded as a token, under its label.
		Name: "token", Actor: Actor{UserID: "user_2", Name: "ci-deploy", Type: actorTypeToken},
		HasActor: true, WantBy: "ci-deploy", WantByType: actorTypeToken,
	}, { // Test 2: An install with no authentication has nobody to name, and the decision records
		// empty rather than inventing an actor.
		Name: "open install", HasActor: false, WantBy: "", WantByType: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			held := heldRun(t, false)
			approver := &stubApprover{result: held}
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/approve", nil)
			req.SetPathValue("id", "run_held")
			if test.HasActor {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			}
			rec := httptest.NewRecorder()
			approveRunHandler(approver, &stubRuns{present: held}, nil, zap.NewNop()).
				ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 (%q)", test.Name, rec.Code, rec.Body.String())
			}
			if approver.gotBy != test.WantBy || approver.gotByType != test.WantByType {
				t.Errorf("%s: decided by %q as %q, want %q as %q",
					test.Name, approver.gotBy, approver.gotByType, test.WantBy, test.WantByType)
			}
			if approver.gotID != "run_held" {
				t.Errorf("%s: decided run = %q, want run_held", test.Name, approver.gotID)
			}
		})
	}
}

// TestApprovalResponseMasksNotificationSecrets pins that the run a decision hands back is redacted
// the same as every other run response. A template's notification targets travel onto every run it
// launches, and a webhook URL is a bearer secret by itself, so a decision endpoint that returned the
// raw target would undo the masking every other route applies.
func TestApprovalResponseMasksNotificationSecrets(t *testing.T) {
	t.Parallel()
	const secretURL = "https://hooks.slack.example/services/T000/B000/SUPER-SECRET-TOKEN"
	const routingKey = "a-pagerduty-routing-key"
	held := heldRun(t, false)
	held.Notifications = []run.NotifyTarget{
		{Kind: "slack", URL: secretURL},
		{Kind: "pagerduty", Key: routingKey},
	}
	for _, decision := range []string{"approve", "reject"} {
		approver := &stubApprover{result: held}
		req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/"+decision, nil)
		req.SetPathValue("id", "run_held")
		rec := httptest.NewRecorder()
		if decision == "approve" {
			approveRunHandler(approver, &stubRuns{present: held}, nil, zap.NewNop()).
				ServeHTTP(rec, req)
		} else {
			rejectRunHandler(approver, &stubRuns{present: held}, nil, zap.NewNop()).
				ServeHTTP(rec, req)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (%q)", decision, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "SUPER-SECRET-TOKEN") {
			t.Errorf("%s: the response carries the raw webhook URL: %s", decision, body)
		}
		if strings.Contains(body, routingKey) {
			t.Errorf("%s: the response carries the raw routing key: %s", decision, body)
		}
		if !strings.Contains(body, "hooks.slack.example") {
			t.Errorf("%s: masking removed the host too, so the operator cannot tell which channel "+
				"it is: %s", decision, body)
		}
		// The stored record must be left alone, so masking a response cannot destroy the real target.
		if held.Notifications[0].URL != secretURL {
			t.Errorf("%s: masking mutated the stored run's notification target", decision)
		}
		if held.Notifications[1].Key != routingKey {
			t.Errorf("%s: masking mutated the stored run's routing key", decision)
		}
	}
}

// TestMaskRunLeavesTheStoredRecordAlone pins the copy-on-mask contract directly. maskRun is applied
// on the way out of several handlers, and one that mutated its argument would redact the in-memory
// run itself, so the next thing to use those targets would deliver to a masked address.
func TestMaskRunLeavesTheStoredRecordAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      *run.Run
		WantNil bool
	}{{ // Test 0: A nil run is returned as nil rather than dereferenced.
		Name: "nil run", In: nil, WantNil: true,
	}, { // Test 1: A run with no targets is returned untouched.
		Name: "no notifications", In: &run.Run{ID: "run_1"},
	}, { // Test 2: A run with targets is copied before its targets are redacted.
		Name: "with notifications", In: &run.Run{
			ID: "run_1",
			Notifications: []run.NotifyTarget{
				{Kind: "webhook", URL: "https://example.test/hook/secret"},
			},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var before string
			if test.In != nil && len(test.In.Notifications) > 0 {
				before = test.In.Notifications[0].URL
			}
			got := maskRun(test.In)
			if test.WantNil {
				if got != nil {
					t.Errorf("%s: maskRun(nil) = %+v, want nil", test.Name, got)
				}
				return
			}
			if before != "" {
				if test.In.Notifications[0].URL != before {
					t.Errorf("%s: maskRun mutated the run it was given", test.Name)
				}
				if got.Notifications[0].URL == before {
					t.Errorf("%s: the returned copy still carries the raw URL", test.Name)
				}
			}
			// The list form must hold the same guarantee, since it is what the run list uses.
			listed := maskRuns([]*run.Run{test.In})
			if len(listed) != 1 {
				t.Fatalf("%s: maskRuns returned %d entries, want 1", test.Name, len(listed))
			}
			if before != "" && test.In.Notifications[0].URL != before {
				t.Errorf("%s: maskRuns mutated the run it was given", test.Name)
			}
		})
	}
}

// TestApprovalOverTheWiredRouterRefusesAnUnknownField pins that the reject route reaches the strict
// decoder through the real router, not only when the handler is called directly. A route wired to a
// lenient path would drop a misspelled reason silently, and the whole point of decoding strictly is
// that no route gets to opt out.
func TestApprovalOverTheWiredRouterRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	held := heldRun(t, false)
	store := run.NewMemStore()
	if err := store.Save(context.Background(), held); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop(),
		WithApprover(&stubApprover{result: held})).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/reject",
		strings.NewReader(`{"resaon":"typo"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a misspelled reason (%q)", rec.Code, rec.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body.Error, "resaon") {
		t.Errorf("the refusal does not name the offending field: %q", body.Error)
	}
}

// TestApprovalsDisabledOverTheWiredRouter pins that an install with no approver answers both
// decision routes with a not found rather than a panic on a nil interface.
func TestApprovalsDisabledOverTheWiredRouter(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"approve", "reject"} {
		rec := serveWith(t, http.MethodPost, "/v1/runs/run_1/"+decision, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 when approvals are not wired", decision, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "approvals not enabled") {
			t.Errorf("%s: body = %q, want it to say approvals are not enabled",
				decision, rec.Body.String())
		}
	}
}

// TestDenySelfApprovalHandlesAMissingRun pins the guard's own edge cases. It runs before the
// dispatcher on every approval, so a nil run or a run carrying no rule must fall through rather than
// refusing, and a caller with no actor must not be compared against anybody.
func TestDenySelfApprovalHandlesAMissingRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Run      *run.Run
		Actor    Actor
		HasActor bool
		WantStop bool
	}{{ // Test 0: No run to compare against, so nothing is refused here.
		Name: "nil run", Run: nil, HasActor: true,
		Actor: Actor{UserID: "user_1"},
	}, { // Test 1: A run whose rule does not demand a second person is not this guard's business.
		Name: "no distinct rule", Run: &run.Run{ID: "run_1", ActorUserID: "user_1"},
		HasActor: true, Actor: Actor{UserID: "user_1"},
	}, { // Test 2: No actor at all, which is an install running open, so there is nobody to compare.
		Name: "no actor", Run: &run.Run{
			ID: "run_1", ActorUserID: "user_1", RequireDistinctApprover: true,
		},
		HasActor: false,
	}, { // Test 3: A different person under the rule passes through.
		Name: "different person", Run: &run.Run{
			ID: "run_1", ActorUserID: "user_1", RequireDistinctApprover: true,
		},
		HasActor: true, Actor: Actor{UserID: "user_2"},
	}, { // Test 4: The requester under the rule is stopped.
		Name: "the requester", Run: &run.Run{
			ID: "run_1", ActorUserID: "user_1", RequireDistinctApprover: true,
		},
		HasActor: true, Actor: Actor{UserID: "user_1"}, WantStop: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/approve", nil)
			if test.HasActor {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			}
			rec := httptest.NewRecorder()
			if got := denySelfApproval(rec, req, zap.NewNop(), test.Run); got != test.WantStop {
				t.Errorf("%s: stopped = %v, want %v", test.Name, got, test.WantStop)
			}
			if test.WantStop && rec.Code != http.StatusConflict {
				t.Errorf("%s: status = %d, want 409", test.Name, rec.Code)
			}
		})
	}
}

// TestApproverInterfaceErrorsAreDistinguished pins that the three dispatcher refusals stay
// distinguishable through errors.Is after the handler has mapped them. A handler that compared error
// text instead would silently start reporting a self-approval refusal as a server error the first
// time the message was reworded.
func TestApproverInterfaceErrorsAreDistinguished(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Err  error
		Want error
	}{{ // Test 0: The not-held refusal.
		Name: "not pending", Err: fmt.Errorf("wrapped: %w", dispatch.ErrNotPendingApproval),
		Want: dispatch.ErrNotPendingApproval,
	}, { // Test 1: The child refusal.
		Name: "child", Err: fmt.Errorf("wrapped: %w", dispatch.ErrChildNotApprovable),
		Want: dispatch.ErrChildNotApprovable,
	}, { // Test 2: The separation-of-duties refusal.
		Name: "self approval", Err: fmt.Errorf("wrapped: %w", dispatch.ErrSelfApproval),
		Want: dispatch.ErrSelfApproval,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !errors.Is(test.Err, test.Want) {
				t.Fatalf("%s: the wrapped error no longer matches its sentinel", test.Name)
			}
			held := heldRun(t, false)
			approver := &stubApprover{result: held, err: test.Err}
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/approve", nil)
			req.SetPathValue("id", "run_held")
			rec := httptest.NewRecorder()
			approveRunHandler(approver, &stubRuns{present: held}, nil, zap.NewNop()).
				ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict {
				t.Errorf("%s: a wrapped sentinel mapped to %d, want 409 (%q)",
					test.Name, rec.Code, rec.Body.String())
			}
		})
	}
}
