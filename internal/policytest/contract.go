// Package policytest provides a shared behavior contract for policy.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package policytest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// Contract runs the policy.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() policy.Store) {
	t.Helper()
	t.Run("save list delete", func(t *testing.T) { testSaveListDelete(t, newStore()) })
	everyFieldSurvivesARoundTrip(t, newStore)
	t.Run("a rego policy is refused rather than stored without its modules", func(t *testing.T) {
		testRegoRefused(t, newStore())
	})
	t.Run("get", func(t *testing.T) { testGet(t, newStore()) })
	t.Run("an exemption round trips and still exempts", func(t *testing.T) {
		testExemptionRoundTrip(t, newStore())
	})
	t.Run("empty list is non-nil", func(t *testing.T) {
		got, err := newStore().List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if got == nil {
			t.Error("List() on an empty store = nil, want a non-nil empty slice")
		}
	})
}

// testGet verifies a saved policy round-trips through Get and a missing id reports ErrNotFound.
func testGet(t *testing.T, store policy.Store) {
	ctx := context.Background()
	// Every field that decides what a rule does is set, because a field a store drops is a rule that
	// silently does something else. The separation-of-duties flag was added to the type and to the API
	// and not to the tables, so on a real install the rule loaded back with the requirement off: the
	// requester could approve their own run, and the record of the rules in force described a rule that
	// did not ask for a second person. Only a store-level check catches that, since an in-memory store
	// round-trips whatever struct it was handed.
	p := &policy.Policy{
		ID: policy.NewID(), Name: "prod-destroy", Tool: "terraform", CommandContains: "destroy",
		InventoryID: "inv_prod", Queue: "dmz", ExcludeDryRun: true, MaxDestroy: 3,
		ActorKind: policy.ActorKindAgent, Actor: "deploy-bot", MinRisk: "high",
		Effect: policy.EffectDeny, RequireDistinctApprover: true, RequireReason: "always",
		CreatedAt: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "prod-destroy" || got.Tool != "terraform" || got.CommandContains != "destroy" ||
		got.InventoryID != "inv_prod" || got.Queue != "dmz" || !got.ExcludeDryRun || got.MaxDestroy != 3 {
		t.Errorf("Get() = %+v, want the saved policy", got)
	}
	if got.ActorKind != policy.ActorKindAgent || got.Actor != "deploy-bot" || got.MinRisk != "high" ||
		got.Effect != policy.EffectDeny {
		t.Errorf("Get() criteria = %+v, want the saved actor, risk floor, and effect", got)
	}
	if !got.RequireDistinctApprover {
		t.Error("Get() lost require_distinct_approver, so the rule loads back allowing the requester " +
			"to approve their own run")
	}
	if got.RequireReason != "always" {
		t.Errorf("Get() require_reason = %q, want always: a rule that loads back without it lets a "+
			"decision through with no reason", got.RequireReason)
	}
	if _, err := store.Get(ctx, "pol_missing"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// testExemptionRoundTrip verifies an exemption from the built-in agent hold loads back as the
// exemption it was saved as. A store that dropped the effect would load it as a rule holding every
// run it names, and one that dropped a criterion would load it exempting more agent runs than it
// was written to, so the loaded rule is asked to judge runs as well as compared field by field.
func testExemptionRoundTrip(t *testing.T, store policy.Store) {
	ctx := context.Background()
	p := &policy.Policy{
		ID: policy.NewID(), Name: "nightly smoke", Tool: "bash", CommandContains: "smoke",
		InventoryID: "inv_lab", Queue: "staging", ActorKind: policy.ActorKindAgent,
		Actor: "triage-bot", Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy,
		CreatedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want the exemption accepted", err)
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != p.ID || listed[0].Effect != policy.EffectExempt {
		t.Fatalf("List() = %+v, want the one exemption listed as an exemption", listed)
	}
	if got.Effect != policy.EffectExempt || got.Tool != "bash" || got.CommandContains != "smoke" ||
		got.InventoryID != "inv_lab" || got.Queue != "staging" ||
		got.ActorKind != policy.ActorKindAgent || got.Actor != "triage-bot" ||
		got.MaxDestroy != policy.DisabledMaxDestroy {
		t.Errorf("Get() = %+v, want the saved exemption", got)
	}
	covered := &run.Run{Actor: "triage-bot", ActorType: policy.ActorKindAgent, Tool: "bash",
		Command: "run smoke tests", InventoryID: "inv_lab", Queue: "staging"}
	if held := policy.Requiring(listed, covered); held != nil {
		t.Errorf("the loaded exemption left the agent's run held by %q", held.Label())
	}
	other := *covered
	other.Actor = "release-bot"
	if held := policy.Requiring(listed, &other); !policy.IsAgentDefault(held) {
		t.Errorf("the loaded exemption covered an agent it does not name: held by %v", held)
	}
	person := *covered
	person.Actor, person.ActorType = "dev-lead", "session"
	if held := policy.Requiring(listed, &person); held != nil {
		t.Errorf("the loaded exemption held a person's run: held by %q", held.Label())
	}
}

// testSaveListDelete verifies policies round-trip, list oldest first, and delete reports a missing id.
func testSaveListDelete(t *testing.T, store policy.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"prod-destroy", "all-terraform"} {
		if err := store.Save(ctx, &policy.Policy{
			ID: policy.NewID(), Name: name, Tool: "terraform", CommandContains: "destroy",
			InventoryID: "inv_prod", ExcludeDryRun: true, MaxDestroy: 5,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 || all[0].Name != "prod-destroy" {
		t.Fatalf("List() = %+v, want two policies oldest first", all)
	}
	if all[0].Tool != "terraform" || all[0].CommandContains != "destroy" ||
		all[0].InventoryID != "inv_prod" || !all[0].ExcludeDryRun || all[0].MaxDestroy != 5 {
		t.Errorf("policy fields did not round-trip: %+v", all[0])
	}

	if err := store.Delete(ctx, all[0].ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	rest, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rest) != 1 || rest[0].Name != "all-terraform" {
		t.Errorf("after delete = %+v, want one policy", rest)
	}
	if err := store.Delete(ctx, "pol_missing"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

// everyFieldSurvivesARoundTrip holds every backend to keeping the whole of a policy.
//
// It exists because one field was not kept, and nothing anywhere said so. Reversibility was added to
// the policy, evaluated by the engine, accepted by the API, listed on the page, and never written to
// a column, so a rule saved as "hold anything that cannot be undone" loaded back holding nothing. The
// API answered 200. The page rendered the rule. The only way to find out was to submit a destructive
// run and watch it execute.
//
// A store that silently drops a field is the worst shape a bug can take in this product, because
// every surface above it keeps reporting success. So rather than listing the fields somebody
// remembers, this sets every one by reflection and requires the store to give them all back. A new
// field on a policy is covered the moment it exists.
func everyFieldSurvivesARoundTrip(t *testing.T, newStore func() policy.Store) {
	t.Helper()
	t.Run("every field of a policy survives a round trip", func(t *testing.T) {
		t.Parallel()
		store := newStore()
		ctx := context.Background()

		// Fields the store assigns or that carry their own meaning, filled deliberately below. The
		// Rego bundle is never stored at all, which testRegoRefused holds every store to instead.
		fixed := map[string]bool{"id": true, "name": true, "created_at": true, "rego": true}

		want := &policy.Policy{
			ID:        "pol_roundtrip",
			Name:      "round trip",
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		}
		rv := reflect.ValueOf(want).Elem()
		rt := rv.Type()
		var set []string
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" || fixed[name] {
				continue
			}
			// Values are chosen only to be distinguishable from a zero value, since what is being
			// tested is whether the store keeps them rather than whether they are meaningful.
			switch f := rv.Field(i); f.Kind() {
			case reflect.String:
				f.SetString(policyProbeString(name))
			case reflect.Bool:
				f.SetBool(true)
			case reflect.Int:
				f.SetInt(3)
			default:
				t.Fatalf("policy field %q has kind %s, which this contract cannot set; teach it "+
					"that kind rather than leaving the field unchecked", name, f.Kind())
			}
			set = append(set, name)
		}
		if len(set) < 5 {
			t.Fatalf("only %d policy fields were exercised, which cannot be right: %v", len(set), set)
		}

		if err := store.Save(ctx, want); err != nil {
			t.Fatalf("save policy: %v", err)
		}
		all, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list policies: %v", err)
		}
		var got *policy.Policy
		for _, p := range all {
			if p.ID == want.ID {
				got = p
			}
		}
		if got == nil {
			t.Fatalf("the saved policy was not listed back")
		}

		update := &policy.Policy{ID: "pol_updatetrip", Name: "before", CreatedAt: want.CreatedAt}
		if err := store.Save(ctx, update); err != nil {
			t.Fatalf("save bare policy: %v", err)
		}
		after := *want
		after.ID = "pol_updatetrip"
		if err := store.Save(ctx, &after); err != nil {
			t.Fatalf("update policy: %v", err)
		}
		updated, err := store.Get(ctx, after.ID)
		if err != nil {
			t.Fatalf("get updated policy: %v", err)
		}
		uv := reflect.ValueOf(updated).Elem()
		av := reflect.ValueOf(&after).Elem()
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" || fixed[name] {
				continue
			}
			if !reflect.DeepEqual(av.Field(i).Interface(), uv.Field(i).Interface()) {
				t.Errorf("policy field %q was updated to %v and came back %v: the update path "+
					"dropped what the insert path keeps, so an edited rule silently keeps its old "+
					"meaning", name, av.Field(i).Interface(), uv.Field(i).Interface())
			}
		}

		gv := reflect.ValueOf(got).Elem()
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" || fixed[name] {
				continue
			}
			if !reflect.DeepEqual(rv.Field(i).Interface(), gv.Field(i).Interface()) {
				t.Errorf("policy field %q was saved as %v and came back %v, so a rule using it is "+
					"stored, listed, and enforced as though it were never set",
					name, rv.Field(i).Interface(), gv.Field(i).Interface())
			}
		}
	})
}

// policyProbeString returns a value valid for the field it is filling, since some are validated on
// the way in and an arbitrary string would be rejected rather than round tripped.
func policyProbeString(field string) string {
	switch field {
	case "min_risk":
		return "high"
	case "reversibility":
		return "irreversible"
	case "effect":
		return policy.EffectDeny
	case "actor_kind":
		return "agent"
	default:
		return "probe-" + field
	}
}

// testRegoRefused holds a store to refusing a Rego policy. A database store has no columns for the
// modules, so accepting one would keep the name and drop the bundle, and a policy with no criteria
// left behind holds every run while the record says a Rego policy decided.
func testRegoRefused(t *testing.T, store policy.Store) {
	t.Helper()
	ctx := context.Background()
	prog, err := policy.CompileRego("", "", []policy.RegoModule{{
		File:   "gate.rego",
		Source: "package switchtender\n\nhold contains \"every run\" if true\n",
	}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	p := &policy.Policy{
		ID: policy.NewID(), Name: "rego gate", MaxDestroy: policy.DisabledMaxDestroy, Rego: prog,
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, p); !errors.Is(err, policy.ErrRegoNotStored) {
		t.Fatalf("Save(rego policy) error = %v, want ErrRegoNotStored", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 0 {
		t.Errorf("List() after a refused save = %d policies, want none", len(all))
	}
}
