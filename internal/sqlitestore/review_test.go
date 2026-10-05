package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/reviewtest"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestReviewReportStoreContract runs the report record contract against SQLite.
func TestReviewReportStoreContract(t *testing.T) {
	t.Parallel()
	reviewtest.Contract(t, func() review.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.ReviewReports()
	})
}

// TestReviewReportsFollowRetention runs the retention check against SQLite.
func TestReviewReportsFollowRetention(t *testing.T) {
	t.Parallel()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reviewtest.PurgeFollowsRuns(t, db.Runs(), db.ReviewReports())
}
