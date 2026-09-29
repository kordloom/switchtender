package backup

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestAFullStoreBacksUpAndRestoresWithNoZeroCount closes the loop the reflection guards cannot:
// data actually traveling.
//
// storecoverage forces a store onto the struct and the parity guard forces a Summary count, but a
// field could still sit on both while gather never reads it or apply never writes it, and the
// fidelity fixture could omit the same kind in lockstep. This walks every Summary count by
// reflection over a fully seeded store set: a zero count after Write means gather is not reading
// that store, and a restore count differing from the backup's means apply is not writing it. A new
// kind wired into the struct but not the data path fails here by field name.
func TestAFullStoreBacksUpAndRestoresWithNoZeroCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)

	var buf bytes.Buffer
	sealer := credential.NewSealer("pass", "salt")
	wrote, err := Write(ctx, src, sealer, &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	restored, err := Read(ctx, freshStores(), sealer, &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	wv := reflect.ValueOf(wrote)
	rv := reflect.ValueOf(restored)
	typ := wv.Type()
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() != reflect.Int {
			continue
		}
		checked++
		name := typ.Field(i).Name
		got := wv.Field(i).Int()
		if got == 0 {
			t.Errorf("backup Summary.%s = 0 over a fully seeded store: gather is not reading "+
				"that store, so the kind silently never leaves the install", name)
		}
		if back := rv.Field(i).Int(); back != got {
			t.Errorf("restore Summary.%s = %d, backup counted %d: apply is not writing what "+
				"gather read", name, back, got)
		}
	}
	if checked < 15 {
		t.Fatalf("only %d int fields walked on Summary; the reflection walk is broken", checked)
	}
}
