package importer

import (
	"context"
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/template"
)

// cbkHeldBinds is a template store that holds the first BindAWX it is given until released, so a
// test can stop one import after it checked the AWX ids and stored its templates, and before it
// binds them.
type cbkHeldBinds struct {
	template.Store
	// reached is closed when the held bind arrives.
	reached chan struct{}
	// release lets the held bind continue.
	release chan struct{}
}

// BindAWX holds the call until released, then binds.
func (s *cbkHeldBinds) BindAWX(ctx context.Context, b template.AWXBinding) error {
	close(s.reached)
	<-s.release
	return s.Store.BindAWX(ctx, b)
}

// TestCallbackBindingRaceWritesTheRefusedImport applies two AWX imports at once, from two
// replicas or from the command line and the API, that give one AWX job template id to two
// different AWX objects.
//
// docs/migration.md promises that a different AWX object claiming an id that is already bound fails
// the import before anything is written, and refuseAWXConflicts says the same. The check reads the
// bindings at the start of Apply and the bind happens after every template is stored, with no
// transaction around either, so two imports both pass the check. The binding itself holds, since
// the conditional upsert lets only the first object have the id, but the import that loses learns
// it only at its bind, after its organizations, projects, inventories, credentials, and templates
// are already stored: it reports ErrAWXConflict and leaves a half-applied import behind, its
// templates marked as answering an AWX address that reaches something else.
func TestCallbackBindingRaceWritesTheRefusedImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	held := &cbkHeldBinds{Store: templates, reached: make(chan struct{}),
		release: make(chan struct{})}
	plan := func(export []byte) *Plan {
		t.Helper()
		p, err := FromAWX(export, importNow)
		if err != nil {
			t.Fatalf("FromAWX() error = %v", err)
		}
		return p
	}
	first := plan(awxAliasExport(`"name": "provision", "allow_callbacks": true, "id": 42`))
	second := plan(awxAliasExport(`"name": "deploy", "allow_callbacks": true, "id": 42`))

	done := make(chan error, 1)
	go func() {
		_, err := first.Apply(ctx, awxApplyStores(held))
		done <- err
	}()
	<-held.reached
	if _, err := second.Apply(ctx, awxApplyStores(templates)); err != nil {
		t.Fatalf("the second import, which checked and bound first, error = %v", err)
	}
	close(held.release)
	err := <-done
	if !errors.Is(err, template.ErrAWXConflict) {
		t.Fatalf("the first import, which lost the id, error = %v, want ErrAWXConflict", err)
	}
	list, lerr := templates.List(ctx)
	if lerr != nil {
		t.Fatalf("List() error = %v", lerr)
	}
	for _, tpl := range list {
		if tpl.Name == "provision" {
			t.Errorf("the import refused with ErrAWXConflict wrote its template %q (%s, "+
				"awx_callback %v), want nothing written by a refused import", tpl.Name, tpl.ID,
				tpl.AWXCallback)
		}
	}
	bound, berr := templates.AWXBindingFor(ctx, 42)
	if berr != nil || !bound.SameObject("Platform", "deploy") {
		t.Errorf("the id's binding after the race = %+v, %v, want it to stay with deploy", bound, berr)
	}
}
