package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
)

// startServe runs serve on db over a loopback listener and stops it once it answers. It returns nil
// for a start that served and shut down cleanly, and serve's error for one that ended before
// anything answered, which is how a refused start ends.
func startServe(t *testing.T, db string) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	old := serveListener
	t.Cleanup(func() { serveListener = old })
	serveListener = ln
	setString(t, &serveDB, db)
	setString(t, &serveAddr, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := testCommand()
	cmd.SetContext(ctx)
	exited := make(chan error, 1)
	go func() { exited <- runServe(cmd, nil) }()
	client := &http.Client{Timeout: probeTimeout}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		select {
		case err := <-exited:
			if err == nil {
				return errors.New("serve returned before anything answered, with no error")
			}
			return err
		default:
		}
		resp, err := client.Get("http://" + ln.Addr().String() + "/v1/runs")
		if err != nil {
			time.Sleep(probeInterval)
			continue
		}
		_ = resp.Body.Close()
		cancel()
		select {
		case err := <-exited:
			return err
		case <-time.After(time.Minute):
			t.Fatal("runServe() did not return once it was stopped")
		}
	}
	cancel()
	<-exited
	return errors.New("nothing answered within a minute")
}

// chainLength counts the entries in db's chain.
func chainLength(t *testing.T, db string) int {
	t.Helper()
	return (&adoptFixture{DB: db, Key: gateKey(t, 7)}).state(t).Entries
}

// newestEntry returns the newest entry in db's chain.
func newestEntry(t *testing.T, db string) *audit.Entry {
	t.Helper()
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	chain, err := bundle.Audits().Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("the chain holds no entry")
	}
	return chain[len(chain)-1]
}

// TestServeRefusesALostIdentity pins that a server over a chain bound to an install whose key it
// does not hold does not start. It used to warn and run unkeyed, checking no seal and passing no
// gate beside replicas that held the key.
func TestServeRefusesALostIdentity(t *testing.T) {
	// Not parallel: runServe reads package-level flag variables and the environment.
	quietServeFlags(t)
	db := tempDB(t)
	mintToken(t, db, "first")
	bound := boundInstall(t, db)
	keyFile := filepath.Join(filepath.Dir(db), audit.IdentityFile)
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove the key: %v", err)
	}
	before := chainLength(t, db)
	err := serveUnder(t, db)
	if !errors.Is(err, errLostIdentity) || !strings.Contains(err.Error(), "start is refused") {
		t.Fatalf("runServe() without the install's key: error = %v, want the start refused", err)
	}
	for _, want := range []string{bound, identity.KeyEnv, filepath.Dir(db), "Signing key lost"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if got := boundInstall(t, db); got != bound {
		t.Errorf("the chain is bound to %s after the refused start, want %s", got, bound)
	}
	if after := chainLength(t, db); after != before {
		t.Errorf("the refused start appended %d entries, want none", after-before)
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("the refused start minted a new key where the install's key was missing")
	}
}

// TestAReplicaWithoutTheSeedIsRefusedBesideKeyedOnes pins the replica the warning used to let run:
// the chain is bound under a seed, and a process started without that seed, and with no key file
// beside the database, is refused rather than run unkeyed beside the processes that hold it.
func TestAReplicaWithoutTheSeedIsRefusedBesideKeyedOnes(t *testing.T) {
	// Not parallel: it sets the environment and runServe reads package-level flag variables.
	quietServeFlags(t)
	db := tempDB(t)
	t.Setenv(identity.KeyEnv, hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	var out, prompt strings.Builder
	if err := adopt(context.Background(), adoptRequest{DB: db, Yes: true, In: strings.NewReader(""),
		Out: &out, Prompt: &prompt}); err != nil {
		t.Fatalf("adopt() under the seed error = %v\n%s", err, out.String())
	}
	bound := boundInstall(t, db)
	if bound == "" {
		t.Fatal("adoption left the chain bound to no install")
	}
	t.Setenv(identity.KeyEnv, "")
	keyFile := filepath.Join(filepath.Dir(db), audit.IdentityFile)
	if _, err := os.Stat(keyFile); err == nil {
		t.Fatal("adopt under the seed wrote a key file, so the replica would not be keyless")
	}
	before := chainLength(t, db)
	err := serveUnder(t, db)
	if !errors.Is(err, errLostIdentity) || !strings.Contains(err.Error(), bound) {
		t.Fatalf("runServe() without the seed: error = %v, want the start refused naming %s",
			err, bound)
	}
	if got := boundInstall(t, db); got != bound {
		t.Errorf("the chain is bound to %s after the refused start, want %s", got, bound)
	}
	if after := chainLength(t, db); after != before {
		t.Errorf("the refused start appended %d entries, want none", after-before)
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("the refused start minted a new key where the seed was missing")
	}
}

// TestTheRestoredKeyStarts pins the remedy the refusal names: the same install's key put back,
// from a copy of the file or as the seed again, starts the server keyed over the chain it signed.
func TestTheRestoredKeyStarts(t *testing.T) {
	// Not parallel: it sets the environment and runServe reads package-level flag variables.
	seed := hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
	tests := []struct {
		// Name names the row.
		Name string
		// Lose binds db's chain under a key, takes the key away, and returns how to put it back.
		Lose func(t *testing.T, db string) func()
	}{{ // Test 0: The key file is removed and its saved bytes written back.
		Name: "file",
		Lose: func(t *testing.T, db string) func() {
			mintToken(t, db, "first")
			keyFile := filepath.Join(filepath.Dir(db), audit.IdentityFile)
			saved, err := os.ReadFile(keyFile)
			if err != nil {
				t.Fatalf("read the key: %v", err)
			}
			if err := os.Remove(keyFile); err != nil {
				t.Fatalf("remove the key: %v", err)
			}
			return func() {
				if err := os.WriteFile(keyFile, saved, 0o600); err != nil {
					t.Fatalf("restore the key: %v", err)
				}
			}
		},
	}, { // Test 1: The seed is unset and set again.
		Name: "seed",
		Lose: func(t *testing.T, db string) func() {
			t.Setenv(identity.KeyEnv, seed)
			var out, prompt strings.Builder
			if err := adopt(context.Background(), adoptRequest{DB: db, Yes: true,
				In: strings.NewReader(""), Out: &out, Prompt: &prompt}); err != nil {
				t.Fatalf("adopt() under the seed error = %v\n%s", err, out.String())
			}
			t.Setenv(identity.KeyEnv, "")
			return func() { t.Setenv(identity.KeyEnv, seed) }
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			quietServeFlags(t)
			db := tempDB(t)
			restore := test.Lose(t, db)
			bound := boundInstall(t, db)
			if bound == "" {
				t.Fatal("the chain is bound to no install")
			}
			if err := serveUnder(t, db); !errors.Is(err, errLostIdentity) {
				t.Fatalf("runServe() without the key: error = %v, want errLostIdentity", err)
			}
			restore()
			if err := startServe(t, db); err != nil {
				t.Fatalf("runServe() with the key restored: error = %v, want it to serve", err)
			}
			if got := boundInstall(t, db); got != bound {
				t.Errorf("the chain is bound to %s after the restored start, want %s", got, bound)
			}
			if got := newestEntry(t, db).InstallID; got != bound {
				t.Errorf("the restored start's newest entry names install %q, want %s", got, bound)
			}
		})
	}
}

// TestAHomelessReplicaOnABoundChainIsRefused pins that a process with no seed and no durable
// identity directory, the ordinary shape of a container on a shared database, reports a chain an
// install has signed as a lost key rather than only as a missing directory, which serve warned on
// and ran past. An unbound chain still reports only the missing directory.
func TestAHomelessReplicaOnABoundChainIsRefused(t *testing.T) {
	// Not parallel: it sets the environment.
	const dsn = "postgres://user:secret@db.internal:5432/switchtender"
	tests := []struct {
		// Bound is the install the chain is bound to, empty for an unbound chain.
		Bound string
		// WantLost reports whether the error must be errLostIdentity.
		WantLost bool
	}{{ // Test 0: A bound chain is a lost key.
		Bound: "in_x", WantLost: true,
	}, { // Test 1: An unbound chain is only a missing directory.
		Bound: "", WantLost: false,
	}}
	for testNum, test := range tests {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("AppData", "")
		t.Setenv(identityDirEnv, "")
		t.Setenv(identity.KeyEnv, "")
		store := audit.NewMemStore()
		if test.Bound != "" {
			binder, ok := store.(audit.InstallBinder)
			if !ok {
				t.Fatal("the memory store does not bind an install")
			}
			binder.BindInstall(test.Bound)
			if err := store.Append(context.Background(), &audit.Entry{ID: audit.NewID(),
				At: time.Now(), Actor: "alice", ActorType: "session", Method: "POST",
				Path: "/v1/projects"}); err != nil {
				t.Fatalf("test %d: Append() error = %v", testNum, err)
			}
		}
		_, err := loadProducerIdentity(context.Background(), store, dsn)
		if !errors.Is(err, errNoIdentityHome) {
			t.Errorf("test %d: error = %v, want errNoIdentityHome", testNum, err)
		}
		if got := errors.Is(err, errLostIdentity); got != test.WantLost {
			t.Errorf("test %d: errors.Is(errLostIdentity) = %t, want %t: %v", testNum, got,
				test.WantLost, err)
		}
		if test.WantLost && !strings.Contains(err.Error(), test.Bound) {
			t.Errorf("test %d: the refusal does not name install %s: %v", testNum, test.Bound, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("test %d: the error quotes the DSN, which carries a password: %v", testNum, err)
		}
	}
}

// TestAnUnboundChainWithoutAKeyIsNotALostKey pins how narrow the refusal is. A fresh SQLite install
// still mints its key, and a shared database whose chain no install has signed still reports the
// missing identity serve warns on and runs past, not a lost key. The serve start of a fresh install
// is held end to end by TestServeBootstrapsTokenOnPublicBind.
func TestAnUnboundChainWithoutAKeyIsNotALostKey(t *testing.T) {
	// Not parallel: it sets the environment.
	tests := []struct {
		// Shared reports whether the database is a shared DSN rather than a SQLite file.
		Shared bool
		// WantMinted reports whether a key file must exist afterward.
		WantMinted bool
		// WantShown is what the error must say, empty for no error.
		WantShown string
	}{{ // Test 0: A fresh SQLite install mints its key.
		Shared: false, WantMinted: true,
	}, { // Test 1: A shared database with no identity is refused a mint, and is not a lost key.
		Shared: true, WantShown: "has no signing identity, so it will not create one",
	}}
	for testNum, test := range tests {
		t.Setenv(identity.KeyEnv, "")
		dir := t.TempDir()
		db := filepath.Join(dir, "switchtender.db")
		if test.Shared {
			db = "postgres://switchtender@db.internal/switchtender"
		}
		_, err := identityForChain(context.Background(), audit.NewMemStore(), db, dir)
		if errors.Is(err, errLostIdentity) {
			t.Errorf("test %d: an unbound chain was reported as a lost key: %v", testNum, err)
		}
		switch {
		case test.WantShown == "" && err != nil:
			t.Errorf("test %d: error = %v, want none", testNum, err)
		case test.WantShown != "" && (err == nil || !strings.Contains(err.Error(), test.WantShown)):
			t.Errorf("test %d: error = %v, want it to say %q", testNum, err, test.WantShown)
		}
		_, statErr := os.Stat(filepath.Join(dir, audit.IdentityFile))
		if minted := statErr == nil; minted != test.WantMinted {
			t.Errorf("test %d: key minted = %t, want %t", testNum, minted, test.WantMinted)
		}
	}
}

// TestALostKeyInstallStartsAgainFromABackup is the path the refusal sends an operator down when no
// copy of the key exists: a backup of the lost-key install restored into a new database starts a
// new install under a new key, whose chain opens with the restore.
func TestALostKeyInstallStartsAgainFromABackup(t *testing.T) {
	// Not parallel: it sets the environment and the commands read package-level flag variables.
	quietServeFlags(t)
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "test-key-material")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "test-salt-material")
	source := tempDB(t)
	mintToken(t, source, "first")
	old := boundInstall(t, source)
	if err := os.Remove(filepath.Join(filepath.Dir(source), audit.IdentityFile)); err != nil {
		t.Fatalf("remove the key: %v", err)
	}
	if err := serveUnder(t, source); !errors.Is(err, errLostIdentity) {
		t.Fatalf("runServe() without the key: error = %v, want errLostIdentity", err)
	}

	archive := filepath.Join(t.TempDir(), "backup.stbak")
	target := filepath.Join(t.TempDir(), "switchtender.db")
	setString(t, &backupDB, source)
	setString(t, &backupOut, archive)
	if err := runBackup(testCommand(), nil); err != nil {
		t.Fatalf("runBackup() on the lost-key install error = %v", err)
	}
	setString(t, &restoreDB, target)
	setString(t, &restoreIn, archive)
	if err := runRestore(testCommand(), nil); err != nil {
		t.Fatalf("runRestore() into a new database error = %v", err)
	}

	if err := startServe(t, target); err != nil {
		t.Fatalf("runServe() on the restored install: error = %v, want it to serve", err)
	}
	bound := boundInstall(t, target)
	if bound == "" || bound == old {
		t.Errorf("the restored install's chain is bound to %q, want a new install, not %q", bound, old)
	}
	if entry := chainEntry(t, target, "/cli/restore"); entry.InstallID != bound {
		t.Errorf("the restore entry names install %q, want the new install %s", entry.InstallID, bound)
	}
}
