package relay_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// newPolicyRelay stands up a relay server over the given policy store and returns a PolicyClient
// dialing it, so a test reads policies the way a relay worker does.
func newPolicyRelay(t *testing.T, policies policy.Store) *relay.PolicyClient {
	t.Helper()
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), testWorkerToken, nil, policies))
	t.Cleanup(ts.Close)
	return relay.NewPolicyClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))
}

// TestRelayWorkerReadsTheRealPolicies checks that a worker across the relay evaluates the
// plan-content gate against the policies actually in force.
//
// The gate runs where the run executes. A worker given no policy store at all silently had no gate,
// so a terraform apply scoped by a destroy threshold was held when the control node claimed it and
// applied straight to production when a worker won the race. Refusing every read instead failed
// closed on every terraform run in the install, including ones no policy would have matched, and
// which outcome you got still depended on which claim loop won. Reading the real policies is what
// makes the answer the same in both places.
func TestRelayWorkerReadsTheRealPolicies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// An install with policies serves them, so the worker gates exactly what the control node does.
	backing := policy.NewMemStore()
	threshold := 5
	p := &policy.Policy{
		ID: "pol_destroy", Name: "large destroy", Tool: "terraform", MaxDestroy: threshold,
	}
	if err := backing.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := newPolicyRelay(t, backing).List(ctx)
	if err != nil {
		t.Fatalf("List() across the relay error = %v", err)
	}
	if len(got) != 1 || got[0].ID != p.ID || got[0].MaxDestroy != threshold {
		t.Fatalf("policies across the relay = %+v, want the stored policy with its threshold", got)
	}

	// An install with no policies configured answers empty rather than erroring. Nothing is gated,
	// which is a different answer from being unable to tell, and the worker must not confuse them.
	none, err := newPolicyRelay(t, nil).List(ctx)
	if err != nil {
		t.Fatalf("List() with no policy store error = %v: an install that gates nothing would "+
			"refuse every terraform run", err)
	}
	if len(none) != 0 {
		t.Errorf("policies = %d, want none", len(none))
	}
}

// TestRelayPolicyClientFailsClosedWhenItCannotAsk checks that an unreachable control node reads as
// an error rather than as an empty policy list. A gate that could not be evaluated has not been
// passed, so the caller must be able to tell the two apart.
func TestRelayPolicyClientFailsClosedWhenItCannotAsk(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), testWorkerToken, nil, nil))
	ts.Close() // Nothing is listening, which is what a severed segment looks like.

	c := relay.NewPolicyClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))
	got, err := c.List(context.Background())
	if err == nil {
		t.Fatalf("List() answered %v with the control node unreachable, so an unevaluated gate "+
			"reads as no gate", got)
	}
	if !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("List() error = %v, want it to wrap ErrUnreachable so callers fail closed", err)
	}
}

// TestRelayPolicyClientRefusesWrites checks a worker cannot author the policies that gate it.
func TestRelayPolicyClientRefusesWrites(t *testing.T) {
	t.Parallel()
	c := newPolicyRelay(t, policy.NewMemStore())
	if err := c.Save(context.Background(), &policy.Policy{ID: "pol_x"}); !errors.Is(err, policy.ErrReadOnly) {
		t.Errorf("Save() error = %v, want ErrReadOnly", err)
	}
	if err := c.Delete(context.Background(), "pol_x"); !errors.Is(err, policy.ErrReadOnly) {
		t.Errorf("Delete() error = %v, want ErrReadOnly", err)
	}
}
