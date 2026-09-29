package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/auth"
)

// TestTheFirstTokenStaysOutOfALogWithNoTerminal pins where the first admin token goes when nobody is
// at a terminal to read it.
//
// A container, a pod, or a service manager keeps stderr as the log, so a never-expiring admin token
// printed there stayed in docker logs and kubectl logs across restarts, and went wherever those
// logs are shipped. With no terminal it goes to a file beside the database, readable by this account
// alone, and the log names the file. It swaps os.Stderr, so it cannot run in parallel.
func TestTheFirstTokenStaysOutOfALogWithNoTerminal(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "switchtender.db")
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	t.Cleanup(func() { _ = bundle.Close() })

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	stderr := os.Stderr
	os.Stderr = write
	berr := bootstrapAdminToken(ctx, bundle, ":8080", db)
	os.Stderr = stderr
	_ = write.Close()
	logged, _ := io.ReadAll(read)
	if berr != nil {
		t.Fatalf("bootstrapAdminToken() error = %v", berr)
	}

	path := filepath.Join(filepath.Dir(db), initialTokenFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no token file at %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file is %v, want 0600", mode)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	plain := strings.TrimSpace(string(body))
	if strings.Contains(string(logged), plain) {
		t.Error("the token reached the log despite no terminal")
	}
	if !strings.Contains(string(logged), path) {
		t.Errorf("the log does not say where the token is:\n%s", logged)
	}
	if _, err := bundle.Tokens().FindByHash(ctx, auth.HashToken(plain)); err != nil {
		t.Errorf("the token in the file is not one the server saved: %v", err)
	}
	for _, hint := range []string{"token new --user <account> --name ci --db " + db,
		"token revoke tok_"} {
		if !strings.Contains(string(logged), hint) {
			t.Errorf("the log does not carry %q, so following it misses this database:\n%s", hint, logged)
		}
	}
}

// TestHintsNameTheServersDatabase pins the --db the printed commands carry. Without it a command run
// elsewhere opened, or created, a different database, and a PostgreSQL DSN carries a password that
// must not be printed.
func TestHintsNameTheServersDatabase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In, Want string
	}{
		{In: "/data/switchtender.db", Want: "--db /data/switchtender.db"},                              // Test 0.
		{In: "/srv/a b/st.db", Want: "--db '/srv/a b/st.db'"},                                          // Test 1.
		{In: "postgres://u:secret@db/st", Want: "--db <this server's database DSN>"},                   // Test 2.
		{In: "postgresql://u:secret@db/st?sslmode=require", Want: "--db <this server's database DSN>"}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := dbFlag(test.In); got != test.Want {
				t.Errorf("dbFlag(%q) = %q, want %q", test.In, got, test.Want)
			}
		})
	}
}

// TestAContainerKeepsItsInitialTokenOutOfItsLog pins where the initial admin token goes. A terminal
// shows it once and keeps nothing, so it is printed there. A container's log keeps what a terminal
// attached to it shows, and the banner said the token was shown only once while docker logs kept it.
func TestAContainerKeepsItsInitialTokenOutOfItsLog(t *testing.T) {
	tests := []struct {
		Channel    string
		IsTerminal bool
		Want       bool
	}{
		{Channel: "", IsTerminal: true, Want: false},                    // Test 0: A terminal prints it.
		{Channel: "", IsTerminal: false, Want: true},                    // Test 1: A service log gets a file.
		{Channel: buildChannelContainer, IsTerminal: true, Want: true},  // Test 2: A container with a terminal.
		{Channel: buildChannelContainer, IsTerminal: false, Want: true}, // Test 3: A container without one.
	}
	for testNum, test := range tests {
		setString(t, &BuildChannel, test.Channel)
		if got := initialTokenToFile(test.IsTerminal); got != test.Want {
			t.Errorf("test %d: initialTokenToFile(%v) with channel %q = %v, want %v", testNum,
				test.IsTerminal, test.Channel, got, test.Want)
		}
	}
}
