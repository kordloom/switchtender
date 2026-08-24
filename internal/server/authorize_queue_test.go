package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/user"
)

// TestQueueIsAuthorizedLikeAnyOtherObject covers the submit side of the queue boundary.
//
// The pool file confines which queues a worker token may lease from, and the code calls that the
// blast radius of the token: compromise the machine in the DMZ and it must not claim a production
// run. Nothing confined which queue a submitter could ask for. Queue was copied off the request body
// onto the run, no authorizer, store, or policy referenced the field, and the claim query filters on
// queue with no owner predicate. So a low-privilege caller could post queue "prod" and have the
// production relay execute their run inside that segment, on a host holding its machine identity,
// its mounted secrets, and its SSH agent. The credential ids on the run were grant-checked; the
// worker's network position was not.
func TestQueueIsAuthorizedLikeAnyOtherObject(t *testing.T) {
	t.Parallel()
	withActor := func(a Actor) context.Context {
		return context.WithValue(context.Background(), actorKey{}, a)
	}
	operator := Actor{UserID: "user_1", Role: user.RoleOperator}
	granted := Actor{UserID: "user_2", Role: user.RoleOperator}
	admin := Actor{UserID: "user_3", Role: user.RoleAdmin}

	grants := &fakeGrants{byObject: map[string][]*grant.Grant{
		grant.QueueObject("prod"): {{Subject: "user_2", Access: grant.AccessUse}},
	}}

	tests := []struct {
		Name    string
		Strict  bool
		Actor   Actor
		Queue   string
		WantErr bool
	}{{ // Test 0: The queue carries grants, and this caller holds none of them.
		Name: "ungranted caller, granted queue", Actor: operator, Queue: "prod", WantErr: true,
	}, { // Test 1: The caller holds a use grant on the queue.
		Name: "granted caller", Actor: granted, Queue: "prod",
	}, { // Test 2: An admin is unrestricted, as everywhere else.
		Name: "admin", Actor: admin, Queue: "prod",
	}, { // Test 3: A queue nobody has granted defers to the role, matching every other object.
		Name: "ungoverned queue, permissive install", Actor: operator, Queue: "canary",
	}, { // Test 4: Under strict grants an ungranted queue is refused, which is the default-deny the
		// boundary needs on an install that has turned isolation on.
		Name: "ungoverned queue, strict install", Strict: true, Actor: operator, Queue: "canary",
		WantErr: true,
	}, { // Test 5: No queue asked for is not an object, so it never denies a run.
		Name: "no queue", Strict: true, Actor: operator, Queue: "",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			authz := &authorizer{grants: grants, strict: test.Strict}
			err := authz.authorizeAll(withActor(test.Actor), grant.AccessUse,
				queueObject(test.Queue))
			if (err != nil) != test.WantErr {
				t.Errorf("%s: authorize queue %q = %v, want error: %v",
					test.Name, test.Queue, err, test.WantErr)
			}
		})
	}
}

// TestQueueObjectNamesAQueue checks the helper produces nothing for an unset queue, so it is skipped
// rather than authorized as an object with an empty name.
func TestQueueObjectNamesAQueue(t *testing.T) {
	t.Parallel()
	if got := queueObject(""); got != "" {
		t.Errorf("queueObject(%q) = %q, want empty so authorizeAll skips it", "", got)
	}
	if got := queueObject("   "); got != "" {
		t.Errorf("queueObject(whitespace) = %q, want empty", got)
	}
	if got := queueObject("prod"); got != "queue:prod" {
		t.Errorf("queueObject(prod) = %q, want queue:prod", got)
	}
	if !grant.ValidObject("queue:prod") {
		t.Error("queue:prod is not a grantable object, so an admin cannot grant a queue")
	}
	if grant.ValidObject("queue:") {
		t.Error("the bare queue prefix is grantable, so one grant would stand for every queue")
	}
}
