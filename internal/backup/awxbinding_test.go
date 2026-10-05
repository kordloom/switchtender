package backup

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/template"
)

// TestRestoreRefusesAnAWXBindingToAnotherObject pins that a restore never hands an AWX job template
// id to a different AWX object than the one the install binds it to. The refusal comes before
// anything is written, so the install is not left half restored, and the binding it held is
// unchanged.
func TestRestoreRefusesAnAWXBindingToAnotherObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	dst := freshStores()
	held := template.AWXBinding{AWXID: 42, TemplateID: "tpl_other", Organization: "Edge",
		Name: "provision", CreatedAt: testFillTime, UpdatedAt: testFillTime}
	if err := dst.Templates.BindAWX(ctx, held); err != nil {
		t.Fatalf("BindAWX() error = %v", err)
	}
	if _, err := Read(ctx, dst, testSealerOnce(), &buf); !errors.Is(err, template.ErrAWXConflict) {
		t.Fatalf("Read() error = %v, want ErrAWXConflict", err)
	}
	if tpls, err := dst.Templates.List(ctx); err != nil || len(tpls) != 0 {
		t.Errorf("a refused restore wrote %d templates (%v), want none", len(tpls), err)
	}
	got, err := dst.Templates.AWXBindingFor(ctx, 42)
	if err != nil || got.TemplateID != "tpl_other" || !got.SameObject("Edge", "provision") {
		t.Errorf("after the refused restore the binding is %+v %v, want it unchanged", got, err)
	}
	if !got.UpdatedAt.Equal(testFillTime) {
		t.Errorf("UpdatedAt = %s, want %s", got.UpdatedAt, testFillTime.Format(time.RFC3339))
	}
}

// restoreRacedTemplates is a template store on which an import binds AWX job template id 42 to a
// different AWX object at the moment the restore binds it, after the restore's check found the id
// free.
type restoreRacedTemplates struct {
	template.Store
	// raced is set once the import has taken the id.
	raced bool
}

// BindAWX lets the import take id 42 first, once, then binds.
func (s *restoreRacedTemplates) BindAWX(ctx context.Context, b template.AWXBinding) error {
	if b.AWXID == 42 && !s.raced {
		s.raced = true
		if err := s.Store.BindAWX(ctx, template.AWXBinding{AWXID: 42, TemplateID: "tpl_import",
			Organization: "Elsewhere", Name: "someone else's"}); err != nil {
			return err
		}
	}
	return s.Store.BindAWX(ctx, b)
}

// TestRestoreRacingAnImportForAnAWXIdWritesNothing restores a backup while an import binds one of
// the backup's AWX job template ids to a different AWX object, between the restore's check of the
// bindings and its bind. The check passed, and the restore learned of the conflict only at its
// bind, after the accounts, tokens, credentials, and templates were all written. Claiming the ids
// before anything is written refuses the restore while there is still nothing to undo, and gives
// back the ids it did claim.
func TestRestoreRacingAnImportForAnAWXIdWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	dst := freshStores()
	plain := dst.Templates
	dst.Templates = &restoreRacedTemplates{Store: plain}
	if _, err := Read(ctx, dst, testSealerOnce(), &buf); !errors.Is(err, template.ErrAWXConflict) {
		t.Fatalf("Read() error = %v, want ErrAWXConflict", err)
	}
	dst.Templates = plain
	if got := testObjectCount(t, ctx, dst); got != 0 {
		t.Errorf("the refused restore wrote %d objects, want none", got)
	}
	bindings, err := plain.AWXBindings(ctx)
	if err != nil {
		t.Fatalf("AWXBindings() error = %v", err)
	}
	if len(bindings) != 1 || bindings[0].AWXID != 42 || bindings[0].Name != "someone else's" {
		t.Errorf("after the refused restore the bindings are %+v, want only the import's id 42",
			bindings)
	}
}
