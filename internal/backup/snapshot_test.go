package backup

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/user"
)

// snapshotStores builds the backup store set from a sqlite bundle, mirroring what the CLI wires.
func snapshotStores(db *sqlitestore.DB) Stores {
	return Stores{
		Credentials: db.Credentials(), Projects: db.Projects(), Templates: db.Templates(),
		Inventories: db.Inventories(), InventorySources: db.InventorySources(),
		Schedules: db.Schedules(), Triggers: db.Triggers(), Users: db.Users(),
		Tokens: db.Tokens(), Teams: db.Teams(), Orgs: db.Orgs(), Grants: db.Grants(),
		CredentialTypes: db.CredentialTypes(), Policies: db.Policies(),
		Snapshot: db,
	}
}

// crossWriteCreds wraps the credential store, the first table gather reads, so a write from a
// second database handle lands exactly between two of the backup's table reads: after the snapshot
// is pinned by the first read, before the users table is read. That is the widest window a real
// concurrent writer has, made deterministic.
type crossWriteCreds struct {
	credential.Store
	// write performs the concurrent write, once.
	write func()
}

// List reads the credentials, then fires the concurrent write before returning.
func (c *crossWriteCreds) List(ctx context.Context) ([]*credential.Credential, error) {
	out, err := c.Store.List(ctx)
	if c.write != nil {
		c.write()
		c.write = nil
	}
	return out, err
}

// TestABackupIsOneInstantNotFourteen pins the backup against a database another process is writing.
//
// gather reads fourteen tables one query at a time, and without a snapshot each query sees its own
// instant. A user created while the backup ran used to land in whichever tables were read after it
// and not the ones read before, producing a backup of a state the database never was in; restoring
// it restores that never-state. With the snapshot pinned, everything the writer did mid-backup is
// visible in none of the tables, and the world it wrote appears whole in the next backup instead
// of split across this one.
func TestABackupIsOneInstantNotFourteen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	a, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("open handle a: %v", err)
	}
	defer func() { _ = a.Close() }()
	// The second handle stands in for the server process writing the same file.
	b, err := sqlitestore.Open(path)
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

	writeMidBackup := func() {
		mid, err := user.New("mid-backup", "correct-horse-battery", user.RoleOperator)
		if err != nil {
			t.Fatalf("user.New: %v", err)
		}
		if err := b.Users().Save(ctx, mid); err != nil {
			t.Fatalf("write mid-backup user through handle b: %v", err)
		}
	}

	stores := snapshotStores(a)
	stores.Credentials = &crossWriteCreds{Store: stores.Credentials, write: writeMidBackup}
	var buf bytes.Buffer
	sum, err := Write(ctx, stores, credential.NewSealer("pass", "salt"), &buf)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sum.Users != 1 {
		t.Errorf("the backup holds %d users, want 1: a write that landed mid-backup was captured "+
			"in the tables read after it, so the backup is a state the database never was in",
			sum.Users)
	}

	// The snapshot ended with the backup: the mid-backup write is whole in the next one.
	var next bytes.Buffer
	sumNext, err := Write(ctx, snapshotStores(a), credential.NewSealer("pass", "salt"), &next)
	if err != nil {
		t.Fatalf("Write after release: %v", err)
	}
	if sumNext.Users != 2 {
		t.Errorf("the next backup holds %d users, want 2: the snapshot outlived its backup and is "+
			"hiding a committed write", sumNext.Users)
	}
}

// TestTheSnapshotHoldsAcrossReadsAndEndsAtRelease exercises the seam directly: a read inside the
// snapshot does not see a write committed after it was pinned, and the release makes it visible.
func TestTheSnapshotHoldsAcrossReadsAndEndsAtRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	a, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("open handle a: %v", err)
	}
	defer func() { _ = a.Close() }()
	b, err := sqlitestore.Open(path)
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
	// The first read pins the snapshot; SQLite takes it at the first statement, not at BEGIN.
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
		t.Errorf("read inside the snapshot sees %d users (err %v), want still 1: the snapshot is "+
			"not holding and a backup gathered through it mixes instants", len(got), err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, err := a.Users().List(ctx); err != nil || len(got) != 2 {
		t.Errorf("read after release sees %d users (err %v), want 2: the snapshot outlived its "+
			"release", len(got), err)
	}
}
