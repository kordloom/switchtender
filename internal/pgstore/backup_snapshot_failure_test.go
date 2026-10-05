package pgstore

import (
	"bytes"
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/backup"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/user"
)

// audCrossWriteCreds wraps the credential store, the first table a backup reads, and fires a write
// from another handle once the credentials have been read, so a concurrent writer lands exactly
// between two of the backup's table reads.
type audCrossWriteCreds struct {
	credential.Store
	// write performs the concurrent write, once.
	write func()
}

// List reads the credentials, then fires the concurrent write before returning.
func (c *audCrossWriteCreds) List(ctx context.Context) ([]*credential.Credential, error) {
	out, err := c.Store.List(ctx)
	if c.write != nil {
		c.write()
		c.write = nil
	}
	return out, err
}

// audBackupStores wires the backup store set from a PostgreSQL bundle the way the backup command
// wires it, the snapshot seam included whenever the bundle offers one.
func audBackupStores(db *DB) backup.Stores {
	stores := backup.Stores{
		Credentials: db.Credentials(), Projects: db.Projects(), Templates: db.Templates(),
		Inventories: db.Inventories(), InventorySources: db.InventorySources(),
		Schedules: db.Schedules(), Triggers: db.Triggers(), Notifications: db.Notifications(),
		Users: db.Users(), Tokens: db.Tokens(), Teams: db.Teams(), Orgs: db.Orgs(),
		Grants: db.Grants(), CredentialTypes: db.CredentialTypes(), Policies: db.Policies(),
	}
	if sb, ok := any(db).(backup.SnapshotBeginner); ok {
		stores.Snapshot = sb
	}
	return stores
}

// TestABackupOfPostgresIsOneInstant is TestABackupIsOneInstantNotFourteen on the backend high
// availability runs on: a backup taken while a replica writes the same PostgreSQL database.
//
// The backup reads its tables one query at a time over a connection pool, and the PostgreSQL
// bundle offers no read snapshot, so every table is read at its own instant. A user created while
// the backup runs lands in whichever tables are read after the write and not the ones before it,
// and the sealed file holds a state the database never was in, which a restore then writes back as
// though it had been. The command prints a note to standard error saying so; the backup page that
// tells operators how to back up and move to PostgreSQL does not, and the PostgreSQL backend is the
// one every highly available install, the one most likely to be written during a backup, runs on.
func TestABackupOfPostgresIsOneInstant(t *testing.T) {
	t.Parallel()
	a, b := audTwoReplicas(t)
	ctx := context.Background()

	seed, err := user.New("existing", "correct-horse-battery", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := a.Users().Save(ctx, seed); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	stores := audBackupStores(a)
	stores.Credentials = &audCrossWriteCreds{Store: stores.Credentials, write: func() {
		mid, err := user.New("mid-backup", "correct-horse-battery", user.RoleOperator)
		if err != nil {
			t.Errorf("user.New: %v", err)
			return
		}
		if err := b.Users().Save(ctx, mid); err != nil {
			t.Errorf("write the mid-backup user through replica b: %v", err)
		}
	}}

	var buf bytes.Buffer
	sum, err := backup.Write(ctx, stores, credential.NewSealer("pass", "salt"), &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if sum.Users != 1 {
		t.Errorf("the backup holds %d users, want 1: a write that landed mid-backup was "+
			"captured in the tables read after it, so the backup is a state the database never "+
			"was in", sum.Users)
	}
}
