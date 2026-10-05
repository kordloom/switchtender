package templatetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// testCallbackSettings verifies the callback limit mode and the AWX-compatible address switch round
// trip through Save and Update, and that a template saved without them reads back with neither.
func testCallbackSettings(t *testing.T, store template.Store) {
	ctx := context.Background()
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &template.Template{ID: "tpl_plain", Name: "plain", Playbook: "p.yml",
		CreatedAt: created}); err != nil {
		t.Fatalf("Save(plain) error = %v", err)
	}
	if err := store.Save(ctx, &template.Template{ID: "tpl_boot", Name: "boot", Playbook: "p.yml",
		InventoryID: "inv_1", AllowCallbacks: true, CallbackLimit: template.CallbackLimitReplace,
		AWXCallback: true, CreatedAt: created}); err != nil {
		t.Fatalf("Save(boot) error = %v", err)
	}
	read := func(id string) *template.Template {
		t.Helper()
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		return got
	}
	if got := read("tpl_plain"); got.CallbackLimit != "" || got.AWXCallback {
		t.Errorf("a template saved without them reads callback_limit %q awx_callback %v",
			got.CallbackLimit, got.AWXCallback)
	}
	got := read("tpl_boot")
	if got.CallbackLimit != template.CallbackLimitReplace || !got.AWXCallback {
		t.Errorf("Get() callback_limit %q awx_callback %v, want replace and on", got.CallbackLimit,
			got.AWXCallback)
	}
	if err := store.Update(ctx, &template.Template{ID: "tpl_boot", Name: "boot", Playbook: "p.yml",
		InventoryID: "inv_1", AllowCallbacks: true,
		CallbackLimit: template.CallbackLimitIntersect}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got = read("tpl_boot")
	if got.CallbackLimit != template.CallbackLimitIntersect || got.AWXCallback {
		t.Errorf("after Update callback_limit %q awx_callback %v, want intersect and off",
			got.CallbackLimit, got.AWXCallback)
	}
}

// testAWXBindings verifies the immutable binding of an AWX job template id: a first bind records
// it, a bind of the same AWX object points it at a new template and keeps when it was first made,
// a different AWX object claiming the id is refused without changing it, and a binding outlives
// its template so the address it serves can answer gone.
func testAWXBindings(t *testing.T, store template.Store) {
	ctx := context.Background()
	first := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	later := first.Add(24 * time.Hour)
	if _, err := store.AWXBindingFor(ctx, 42); !errors.Is(err, template.ErrNotFound) {
		t.Fatalf("AWXBindingFor(unbound) error = %v, want ErrNotFound", err)
	}
	bound := template.AWXBinding{AWXID: 42, TemplateID: "tpl_a", Organization: "Platform",
		Name: "provision", CreatedAt: first, UpdatedAt: first}
	if err := store.BindAWX(ctx, bound); err != nil {
		t.Fatalf("BindAWX(first) error = %v", err)
	}
	read := func() *template.AWXBinding {
		t.Helper()
		got, err := store.AWXBindingFor(ctx, 42)
		if err != nil {
			t.Fatalf("AWXBindingFor() error = %v", err)
		}
		return got
	}
	if diff := cmp.Diff(bound, *read()); diff != "" {
		t.Errorf("first binding mismatch (-want +got):\n%s", diff)
	}

	tests := []struct {
		Want    error
		Binding template.AWXBinding
	}{{ // Test 0: Another organization's template of the same name is a different object.
		Binding: template.AWXBinding{AWXID: 42, TemplateID: "tpl_b", Organization: "Edge",
			Name: "provision", CreatedAt: later, UpdatedAt: later},
		Want: template.ErrAWXConflict,
	}, { // Test 1: Another template of the same organization is a different object.
		Binding: template.AWXBinding{AWXID: 42, TemplateID: "tpl_b", Organization: "Platform",
			Name: "deploy", CreatedAt: later, UpdatedAt: later},
		Want: template.ErrAWXConflict,
	}}
	for testNum, test := range tests {
		if err := store.BindAWX(ctx, test.Binding); !errors.Is(err, test.Want) {
			t.Errorf("test %d: BindAWX() error = %v, want %v", testNum, err, test.Want)
		}
		if got := read(); got.TemplateID != "tpl_a" || !got.SameObject("Platform", "provision") {
			t.Errorf("test %d: a refused bind changed the binding to %+v", testNum, got)
		}
	}

	// A re-import of the same AWX object points the id at the template it created.
	if err := store.BindAWX(ctx, template.AWXBinding{AWXID: 42, TemplateID: "tpl_c",
		Organization: "Platform", Name: "provision", CreatedAt: later, UpdatedAt: later}); err != nil {
		t.Fatalf("BindAWX(same object) error = %v", err)
	}
	moved := read()
	if moved.TemplateID != "tpl_c" || !moved.CreatedAt.Equal(first) || !moved.UpdatedAt.Equal(later) {
		t.Errorf("after a re-import the binding is %+v, want tpl_c first bound %s and updated %s",
			moved, first, later)
	}

	at := later.Add(time.Hour)
	if err := store.MarkAWXCalled(ctx, 42, at); err != nil {
		t.Fatalf("MarkAWXCalled() error = %v", err)
	}
	if got := read(); got.LastCalledAt == nil || !got.LastCalledAt.Equal(at) {
		t.Errorf("LastCalledAt = %v, want %s", got.LastCalledAt, at)
	}
	if err := store.MarkAWXCalled(ctx, 7, at); !errors.Is(err, template.ErrNotFound) {
		t.Errorf("MarkAWXCalled(unbound) error = %v, want ErrNotFound", err)
	}

	// A binding outlives the template it reached, so its address can say the template is gone.
	if err := store.Save(ctx, &template.Template{ID: "tpl_c", Name: "provision", Playbook: "p.yml",
		CreatedAt: first}); err != nil {
		t.Fatalf("Save(tpl_c) error = %v", err)
	}
	if err := store.Delete(ctx, "tpl_c"); err != nil {
		t.Fatalf("Delete(tpl_c) error = %v", err)
	}
	if got := read(); got.TemplateID != "tpl_c" {
		t.Errorf("after the template was deleted the binding reaches %q, want the deleted tpl_c",
			got.TemplateID)
	}

	if err := store.BindAWX(ctx, template.AWXBinding{AWXID: 7, TemplateID: "tpl_d",
		Name: "no organization", CreatedAt: first, UpdatedAt: first}); err != nil {
		t.Fatalf("BindAWX(7) error = %v", err)
	}
	all, err := store.AWXBindings(ctx)
	if err != nil {
		t.Fatalf("AWXBindings() error = %v", err)
	}
	var ids []int64
	for _, b := range all {
		ids = append(ids, b.AWXID)
	}
	if diff := cmp.Diff([]int64{7, 42}, ids, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("AWXBindings() ids mismatch (-want +got):\n%s", diff)
	}
}

// testAWXUnbind verifies an unbind removes a binding only while it still names the AWX object and
// the template the caller claimed it for. An import that claimed an id and then stopped gives it
// back this way, and must never take back an id another import has since pointed at its own
// template, or one a different AWX object holds.
func testAWXUnbind(t *testing.T, store template.Store) {
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	claim := template.AWXBinding{AWXID: 42, TemplateID: "tpl_a", Organization: "Platform",
		Name: "provision", CreatedAt: at, UpdatedAt: at}
	if err := store.BindAWX(ctx, claim); err != nil {
		t.Fatalf("BindAWX() error = %v", err)
	}
	tests := []struct {
		Unbind    template.AWXBinding
		WantBound bool
	}{{ // Test 0: Another template of the same object leaves the binding alone.
		Unbind: template.AWXBinding{AWXID: 42, TemplateID: "tpl_b", Organization: "Platform",
			Name: "provision"},
		WantBound: true,
	}, { // Test 1: Another object leaves the binding alone.
		Unbind: template.AWXBinding{AWXID: 42, TemplateID: "tpl_a", Organization: "Edge",
			Name: "provision"},
		WantBound: true,
	}, { // Test 2: The claim itself is given back.
		Unbind: claim, WantBound: false,
	}, { // Test 3: Giving back an id nobody holds changes nothing and is not an error.
		Unbind: claim, WantBound: false,
	}}
	for testNum, test := range tests {
		if err := store.UnbindAWX(ctx, test.Unbind); err != nil {
			t.Fatalf("test %d: UnbindAWX() error = %v", testNum, err)
		}
		_, err := store.AWXBindingFor(ctx, 42)
		if bound := err == nil; bound != test.WantBound {
			t.Errorf("test %d: after UnbindAWX() the id is bound %t (%v), want %t", testNum, bound,
				err, test.WantBound)
		}
	}
}
