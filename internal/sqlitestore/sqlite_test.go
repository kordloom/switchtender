package sqlitestore_test

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/sqlitestore"
	"github.com/dcadolph/yardmaster/internal/storetest"
)

func TestStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store {
		store, err := sqlitestore.New(filepath.Join(t.TempDir(), "yardmaster.db"))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if c, ok := store.(io.Closer); ok {
			t.Cleanup(func() { _ = c.Close() })
		}
		return store
	})
}
