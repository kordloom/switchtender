package importer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/template"
)

// claimRacedStore is a template store on which another import binds AWX job template id 42 to a
// different AWX object at the moment this import binds it, after this import's check found the id
// free. A template save can be made to fail, for the import that stops partway.
type claimRacedStore struct {
	template.Store
	// raceOn is the id another import takes between this import's check and its bind, zero for none.
	raceOn int64
	// failSave fails every template save when set.
	failSave error
}

// BindAWX lets another import take raceOn first, then binds.
func (s *claimRacedStore) BindAWX(ctx context.Context, b template.AWXBinding) error {
	if b.AWXID == s.raceOn {
		s.raceOn = 0
		if err := s.Store.BindAWX(ctx, template.AWXBinding{AWXID: b.AWXID, TemplateID: "tpl_other",
			Organization: "Elsewhere", Name: "someone else's"}); err != nil {
			return err
		}
	}
	return s.Store.BindAWX(ctx, b)
}

// Save fails when failSave is set.
func (s *claimRacedStore) Save(ctx context.Context, t *template.Template) error {
	if s.failSave != nil {
		return s.failSave
	}
	return s.Store.Save(ctx, t)
}

// TestARefusedImportGivesBackTheAWXIdsItClaimed applies imports that claim two AWX job template ids
// and then stop: one because another import took its second id at the bind, one because a template
// would not save. Claiming the ids before writing anything is what keeps a losing import from
// leaving half of itself behind, and that only holds if an import that stops also gives back the
// ids it did claim, since an id bound for good to a template that was never made answers gone for
// an AWX object nobody imported.
func TestARefusedImportGivesBackTheAWXIdsItClaimed(t *testing.T) {
	t.Parallel()
	errDisk := errors.New("disk full")
	tests := []struct {
		RaceOn       int64
		FailSave     error
		Want         error
		WantBindings int
	}{{ // Test 0: The second id is lost to another import, which keeps it, and the first goes back.
		RaceOn: 42, Want: template.ErrAWXConflict, WantBindings: 1,
	}, { // Test 1: A template does not save, so both ids go back.
		FailSave: errDisk, Want: errDisk, WantBindings: 0,
	}, { // Test 2: Nothing stops the import, so both ids stay bound to its templates.
		WantBindings: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			plan, err := FromAWX(awxAliasExport(
				`"name": "build", "allow_callbacks": true, "id": 41`,
				`"name": "provision", "allow_callbacks": true, "id": 42`), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			templates := template.NewMemStore()
			store := &claimRacedStore{Store: templates, raceOn: test.RaceOn,
				failSave: test.FailSave}
			_, err = plan.Apply(ctx, awxApplyStores(store))
			if !errors.Is(err, test.Want) {
				t.Fatalf("Apply() error = %v, want %v", err, test.Want)
			}
			bindings, err := templates.AWXBindings(ctx)
			if err != nil {
				t.Fatalf("AWXBindings() error = %v", err)
			}
			if len(bindings) != test.WantBindings {
				t.Errorf("the import left %d AWX bindings, want %d: %+v", len(bindings),
					test.WantBindings, bindings)
			}
			for _, b := range bindings {
				if b.AWXID == 41 && b.Name != "build" {
					t.Errorf("id 41 is bound to %q, want the import's own build or nothing", b.Name)
				}
				if test.RaceOn != 0 && b.AWXID == 42 && b.Name != "someone else's" {
					t.Errorf("id 42 is bound to %q, want it kept by the import that won it", b.Name)
				}
			}
			if test.RaceOn != 0 {
				list, err := templates.List(ctx)
				if err != nil {
					t.Fatalf("List() error = %v", err)
				}
				if len(list) != 0 {
					t.Errorf("the refused import stored %d templates, want none", len(list))
				}
			}
		})
	}
}
