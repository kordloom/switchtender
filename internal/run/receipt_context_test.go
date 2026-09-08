package run

import (
	"context"
	"fmt"
	"testing"
)

// TestAuditReceiptRidesTheContext pins how the receipt of the request in flight is carried to a run
// created while handling it.
//
// The chain entry is appended before the handler runs and names the request path, which cannot name
// a run that does not exist yet, so passing the receipt forward on the context is the only thing
// tying a run to the record of who asked for it. A lost receipt leaves the run with no provenance
// and nothing failing to say so.
func TestAuditReceiptRidesTheContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Receipts    []string
		WantReceipt string
	}{
		{Name: "nothing set", Receipts: nil, WantReceipt: ""}, // Test 0: No request.
		{Name: "one receipt", Receipts: []string{"41:9f2caa"},
			WantReceipt: "41:9f2caa"}, // Test 1: The ordinary case.
		{Name: "nested overrides", Receipts: []string{"41:9f2caa", "42:aa11bb"},
			WantReceipt: "42:aa11bb"}, // Test 2: The innermost request wins.
		{Name: "empty does not clear", Receipts: []string{"41:9f2caa", ""},
			WantReceipt: "41:9f2caa"}, // Test 3: An empty receipt leaves the outer one in place
		// rather than blanking the provenance of everything downstream.
		{Name: "empty on a bare context", Receipts: []string{""},
			WantReceipt: ""}, // Test 4: Still nothing.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			for _, r := range test.Receipts {
				ctx = WithAuditReceipt(ctx, r)
			}
			if got := AuditReceiptFrom(ctx); got != test.WantReceipt {
				t.Errorf("AuditReceiptFrom() = %q, want %q", got, test.WantReceipt)
			}
		})
	}
}

// TestSubmitterOrgRidesTheContext pins the same carriage for the submitting actor's organization,
// which is what scopes a run that references no stored object.
//
// An inline script, a proposed run, or a terraform working directory names no project, inventory,
// or credential, so there is nothing for the per-object grant check to filter on. Without the org
// stamped from the context, such a run is readable, cancelable, and approvable across every tenant
// on the install.
func TestSubmitterOrgRidesTheContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Orgs    []string
		WantOrg string
	}{
		{Name: "nothing set", Orgs: nil, WantOrg: ""},                      // Test 0: No actor.
		{Name: "one org", Orgs: []string{"org_acme"}, WantOrg: "org_acme"}, // Test 1: Ordinary.
		{Name: "nested overrides", Orgs: []string{"org_acme", "org_globex"},
			WantOrg: "org_globex"}, // Test 2: The inner scope wins.
		{Name: "empty does not clear", Orgs: []string{"org_acme", ""},
			WantOrg: "org_acme"}, // Test 3: An empty org must not silently unown a run and make
		// it visible to every tenant.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			for _, o := range test.Orgs {
				ctx = WithSubmitterOrg(ctx, o)
			}
			if got := SubmitterOrgFrom(ctx); got != test.WantOrg {
				t.Errorf("SubmitterOrgFrom() = %q, want %q", got, test.WantOrg)
			}
		})
	}
}

// TestTheReceiptAndOrgKeysDoNotCollide pins that the two context values are stored under distinct
// keys, so neither can be read as the other.
//
// They are set at the same point in the auth gate and read by the same submit path. If they shared
// a key, whichever was set second would answer both questions: the tenant scoping a run would come
// back as an audit receipt, or every run would be stamped with an organization named after a chain
// entry, which is a tenancy boundary decided by a typo.
func TestTheReceiptAndOrgKeysDoNotCollide(t *testing.T) {
	t.Parallel()
	ctx := WithSubmitterOrg(WithAuditReceipt(context.Background(), "41:9f2caa"), "org_acme")
	if got := AuditReceiptFrom(ctx); got != "41:9f2caa" {
		t.Errorf("AuditReceiptFrom() = %q, want the receipt unaffected by the org", got)
	}
	if got := SubmitterOrgFrom(ctx); got != "org_acme" {
		t.Errorf("SubmitterOrgFrom() = %q, want the org unaffected by the receipt", got)
	}

	// Setting them in the other order gives the same answers.
	ctx = WithAuditReceipt(WithSubmitterOrg(context.Background(), "org_acme"), "41:9f2caa")
	if AuditReceiptFrom(ctx) != "41:9f2caa" || SubmitterOrgFrom(ctx) != "org_acme" {
		t.Errorf("order changed the answers: receipt %q, org %q",
			AuditReceiptFrom(ctx), SubmitterOrgFrom(ctx))
	}

	// A value another package stored under its own key is not mistaken for either of these.
	type foreignKey struct{}
	ctx = context.WithValue(context.Background(), foreignKey{}, "not mine")
	if AuditReceiptFrom(ctx) != "" || SubmitterOrgFrom(ctx) != "" {
		t.Errorf("another package's context value read as receipt %q / org %q",
			AuditReceiptFrom(ctx), SubmitterOrgFrom(ctx))
	}
}

// TestContextHelpersSurviveACanceledContext pins that reading either value still works after the
// request's context is canceled, since a run being finished after a client disconnects still has to
// record which request created it.
func TestContextHelpersSurviveACanceledContext(t *testing.T) {
	t.Parallel()
	ctx := WithSubmitterOrg(WithAuditReceipt(context.Background(), "41:9f2caa"), "org_acme")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if AuditReceiptFrom(canceled) != "41:9f2caa" || SubmitterOrgFrom(canceled) != "org_acme" {
		t.Errorf("a canceled context lost its values: receipt %q, org %q",
			AuditReceiptFrom(canceled), SubmitterOrgFrom(canceled))
	}
}
