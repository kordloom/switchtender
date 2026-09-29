package cmd

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestBackupStoresWiresEveryField pins the last hand-list in the backup chain: the CLI wiring.
//
// The struct is guarded, the counts are guarded, and the data path is guarded, all inside the
// backup package. The one remaining way to lose a store was here: backupStores maps fourteen
// bundle accessors into the struct by hand, and a store added everywhere else but not wired here
// backs up nothing from a real database while every in-package test stays green. A nil field on
// the wired struct is exactly that omission, named.
func TestBackupStoresWiresEveryField(t *testing.T) {
	t.Parallel()
	bundle, err := openBundle(filepath.Join(t.TempDir(), "wiring.db"))
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	stores := backupStores(bundle)
	v := reflect.ValueOf(stores)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Interface {
			continue
		}
		if f.IsNil() {
			t.Errorf("backupStores leaves Stores.%s nil for a SQLite bundle: that store is "+
				"silently absent from every backup the CLI writes", typ.Field(i).Name)
		}
	}
}
