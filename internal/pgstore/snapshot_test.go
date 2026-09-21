package pgstore_test

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheReadSnapshotHoldsAgainstAConcurrentWriter proves the seam a backup rests on: a read
// inside the snapshot does not see a row committed after it began, and the release both surfaces
// the row and reports that the snapshot held. It runs against the same live database as the rest
// of the contract, because REPEATABLE READ semantics are the server's to prove, not the driver's.
func TestTheReadSnapshotHoldsAgainstAConcurrentWriter(t *testing.T) {
	dsn := testDSN(t)
	truncateAll(t, dsn)
	ctx := context.Background()

	a, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("open handle a: %v", err)
	}
	defer func() { _ = a.Close() }()
	// The second handle stands in for the server writing while the backup CLI reads.
	b, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("open handle b: %v", err)
	}
	defer func() { _ = b.Close() }()

	seed, err := user.New("existing", "correct-horse-battery", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := a.Users().Save(ctx, seed); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	release, err := a.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	if got, err := a.Users().List(ctx); err != nil || len(got) != 1 {
		t.Fatalf("first read inside the snapshot: %d users, err %v; want 1", len(got), err)
	}
	mid, err := user.New("after-pin", "correct-horse-battery", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := b.Users().Save(ctx, mid); err != nil {
		t.Fatalf("write through handle b: %v", err)
	}
	if got, err := a.Users().List(ctx); err != nil || len(got) != 1 {
		t.Errorf("read inside the snapshot sees %d users (err %v), want still 1: a backup "+
			"gathered through this snapshot mixes instants", len(got), err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, err := a.Users().List(ctx); err != nil || len(got) != 2 {
		t.Errorf("read after release sees %d users (err %v), want 2", len(got), err)
	}
}
