package pgstore_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/reviewtest"
)

// TestReviewReportStoreContract runs the report record contract against PostgreSQL.
func TestReviewReportStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reviewtest.Contract(t, func() review.Store {
		truncateTable(t, dsn, "review_reports")
		return db.ReviewReports()
	})
}

// TestReviewReportsFollowRetention runs the retention check against PostgreSQL.
func TestReviewReportsFollowRetention(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	truncateTable(t, dsn, "review_reports")
	reviewtest.PurgeFollowsRuns(t, db.Runs(), db.ReviewReports())
}
