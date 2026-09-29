package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDesktopPortRoundtrip covers saving and reading the persisted desktop port, plus the invalid
// cases: a missing file, garbage content, and an out-of-range value.
func TestDesktopPortRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Test 0: No file reports no port.
	if _, ok := savedDesktopPort(dir); ok {
		t.Error("savedDesktopPort() = ok with no file, want none")
	}

	// Test 1: A saved port reads back.
	saveDesktopPort(dir, 8443)
	port, ok := savedDesktopPort(dir)
	if !ok || port != 8443 {
		t.Errorf("savedDesktopPort() = %d, %v, want 8443, true", port, ok)
	}

	// Test 2: Garbage content reports no port.
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte("not a port"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, ok := savedDesktopPort(dir); ok {
		t.Error("savedDesktopPort() = ok for garbage, want none")
	}

	// Test 3: An out-of-range value reports no port.
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte("70000"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, ok := savedDesktopPort(dir); ok {
		t.Error("savedDesktopPort() = ok for out-of-range, want none")
	}
}

// TestDesktopListener proves the listener reuses the saved port when it is free, records a fresh
// port when there is none, and falls back to a new port when the saved one is taken.
func TestDesktopListener(t *testing.T) {
	t.Parallel()

	// Test 0: With no saved port, a fresh one is bound and recorded.
	dir := t.TempDir()
	l, err := desktopListener(dir)
	if err != nil {
		t.Fatalf("desktopListener() error = %v", err)
	}
	first := l.Addr().(*net.TCPAddr).Port
	saved, ok := savedDesktopPort(dir)
	if !ok || saved != first {
		t.Errorf("saved port = %d, %v, want %d, true", saved, ok, first)
	}
	_ = l.Close()

	// Test 1: The saved port is reused when free.
	//
	// "When free" is the whole claim, and the port stopped being this test's the moment it closed
	// the listener above. Eight other tests in this package bind ports and all of them run in
	// parallel, so one can take the freed port in between and the reuse would correctly fall back.
	// Asserting the port outright made this test fail for a reason that is not a defect, which is
	// worse than useless in a gate that blocks releases. So a different port is accepted only on
	// proof that the saved one really was occupied.
	l2, err := desktopListener(dir)
	if err != nil {
		t.Fatalf("desktopListener() reuse error = %v", err)
	}
	if got := l2.Addr().(*net.TCPAddr).Port; got != first {
		probe, perr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first))
		if perr == nil {
			_ = probe.Close()
			t.Errorf("reused port = %d, want %d, and %d was free", got, first, first)
		} else {
			t.Logf("port %d was taken by something else between the close and the reuse, so the "+
				"fallback to %d is the documented behavior rather than a failure", first, got)
		}
	}
	_ = l2.Close()

	// Test 2: A taken saved port falls back to a fresh one.
	//
	// The precondition is that the saved port is occupied. Usually this test occupies it; sometimes
	// another test in this package, all of which run in parallel and bind ports, already has. Both
	// satisfy the precondition, so a bind that fails with the port already in use is the setup
	// succeeding by another route rather than a reason to fail the test.
	blocker, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first))
	switch {
	case err == nil:
		defer func() { _ = blocker.Close() }()
	case strings.Contains(err.Error(), "address already in use"):
		t.Logf("port %d was already taken by another test, which is the state this case needs", first)
	default:
		t.Fatalf("Listen() blocker error = %v", err)
	}
	l3, err := desktopListener(dir)
	if err != nil {
		t.Fatalf("desktopListener() fallback error = %v", err)
	}
	if got := l3.Addr().(*net.TCPAddr).Port; got == first {
		t.Errorf("fallback picked the taken port %d", got)
	}
	_ = l3.Close()
}

// TestDesktopAlive covers the liveness probe: false with nothing listening on the port.
func TestDesktopAlive(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	if desktopAlive(port) {
		t.Errorf("desktopAlive(%d) = true with nothing listening", port)
	}
}

// TestNewDatabaseNoteSparesDesktop pins when serve warns about a database it is about to create.
// Desktop makes its database on first launch by design and takes no --db, so its user was told to
// check a flag they cannot pass.
func TestNewDatabaseNoteSparesDesktop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing := filepath.Join(dir, "there.db")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	missing := filepath.Join(dir, "missing.db")
	tests := []struct {
		DB       string
		Desktop  bool
		WantNote bool
	}{{ // Test 0: serve about to create a database says so, in case --db was mistyped.
		DB: missing, WantNote: true,
	}, { // Test 1: desktop creating its own database says nothing.
		DB: missing, Desktop: true,
	}, { // Test 2: an existing database needs no note.
		DB: existing,
	}, { // Test 3: a PostgreSQL DSN is never a file to check.
		DB: "postgres://u:p@db/st",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			note := newDatabaseNote(test.DB, test.Desktop)
			if (note != "") != test.WantNote {
				t.Errorf("newDatabaseNote(%q, %v) = %q, want a note = %v", test.DB, test.Desktop, note, test.WantNote)
			}
			if test.WantNote && !strings.Contains(note, "check --db") {
				t.Errorf("the note %q does not point at --db", note)
			}
		})
	}
}
