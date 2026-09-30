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
//
// Every test in this package binds ports in parallel, and so does every package in a full run, so a
// port this test frees can be taken before the listener binds it again. The cases used to settle
// whether that had happened with a bind of their own made afterward, and that bind raced whatever
// took the port letting it go again. A correct fallback then failed as a missed reuse, and a
// blocker that could not bind left the fallback case running against a port that was free again by
// the time the listener tried it. Now a port a case needs free or taken is one the test holds for
// the whole case, and the one case that frees a port reads the outcome from the bind the listener
// made itself rather than from a later probe.
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

	// Test 1: The saved port is tried first, a bind on it that succeeds is the listener returned,
	// and the record is left alone.
	//
	// The bind on the saved port is answered with a listener this test already holds on that port,
	// which is what binding a free port gives, so nothing else can take the port in between.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	heldDir := t.TempDir()
	saveDesktopPort(heldDir, port)
	var asked []string
	handOver := func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if address == "127.0.0.1:"+strconv.Itoa(port) {
			return held, nil
		}
		return net.Listen(network, address)
	}
	reused, err := desktopListenerWith(heldDir, handOver)
	if err != nil {
		t.Fatalf("desktopListener() reuse error = %v", err)
	}
	defer func() { _ = reused.Close() }()
	if reused != held || len(asked) != 1 {
		t.Errorf("binds asked for %v and the listener is on %v, want only the saved port %d and "+
			"its listener", asked, reused.Addr(), port)
	}
	if saved, _ := savedDesktopPort(heldDir); saved != port {
		t.Errorf("reusing port %d rewrote the record to %d", port, saved)
	}

	// Test 2: The same reuse with the operating system answering, on the port Test 0 freed.
	//
	// The listener's own bind on the saved port says whether it was still free. Free, the listener
	// must be on it after that one bind. Taken by something else in between, the listener must be
	// on whatever port its next bind was given and have recorded that one. That can be the saved
	// port number again, when the other socket let go and the system handed the number back out.
	// A fallback is correct and is logged rather than failed, since Test 1 already pins the reuse.
	var binds []desktopBind
	l2, err := desktopListenerWith(dir, recordBinds(&binds))
	if err != nil {
		t.Fatalf("desktopListener() reuse error = %v", err)
	}
	defer func() { _ = l2.Close() }()
	got := l2.Addr().(*net.TCPAddr).Port
	want := "127.0.0.1:" + strconv.Itoa(first)
	if len(binds) == 0 || binds[0].Address != want {
		t.Fatalf("binds = %v, want the saved port %s tried first", binds, want)
	}
	saved, _ = savedDesktopPort(dir)
	switch {
	case binds[0].Err == nil && (got != first || len(binds) != 1):
		t.Errorf("the bind on saved port %d succeeded, yet the listener is on %d after %d bind(s)",
			first, got, len(binds))
	case binds[0].Err == nil && saved != first:
		t.Errorf("reusing port %d rewrote the record to %d", first, saved)
	case binds[0].Err != nil && (len(binds) != 2 || binds[1].Address != "127.0.0.1:0" ||
		binds[1].Port != got):
		t.Errorf("the bind on saved port %d was refused, so the listener must be on the fresh "+
			"port it bound next, but it is on %d after binds %v", first, got, binds)
	case binds[0].Err != nil && saved != got:
		t.Errorf("after falling back to %d the record holds %d, want %d", got, saved, got)
	case binds[0].Err != nil:
		t.Logf("port %d was taken before the listener bound it again (%v), so it fell back to %d",
			first, binds[0].Err, got)
	}

	// Test 3: A taken saved port falls back to a fresh one, and the fresh one is recorded.
	//
	// The saved port is one this case binds itself and holds until it ends, so it is taken when the
	// listener tries it whatever else is running.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() blocker error = %v", err)
	}
	defer func() { _ = blocker.Close() }()
	taken := blocker.Addr().(*net.TCPAddr).Port
	takenDir := t.TempDir()
	saveDesktopPort(takenDir, taken)
	binds = nil
	l3, err := desktopListenerWith(takenDir, recordBinds(&binds))
	if err != nil {
		t.Fatalf("desktopListener() fallback error = %v", err)
	}
	defer func() { _ = l3.Close() }()
	want = "127.0.0.1:" + strconv.Itoa(taken)
	got = l3.Addr().(*net.TCPAddr).Port
	if len(binds) != 2 || binds[0].Address != want || binds[0].Err == nil ||
		binds[1].Address != "127.0.0.1:0" || binds[1].Port != got {
		t.Errorf("binds = %v with the listener on %d, want a refused bind on the saved port %s "+
			"and then the fresh port the listener is on", binds, got, want)
	}
	if got == taken {
		t.Errorf("fallback picked the taken port %d", got)
	}
	if saved, _ := savedDesktopPort(takenDir); saved != got {
		t.Errorf("after falling back to %d the record holds %d, want %d", got, saved, got)
	}
}

// desktopBind is one bind the desktop listener attempted.
type desktopBind struct {
	// Address is the host and port the bind asked for.
	Address string
	// Port is the port the bind was given, zero when it failed.
	Port int
	// Err is what the bind returned, nil when it succeeded.
	Err error
}

// recordBinds returns a listenFunc that binds with net.Listen and appends every attempt to binds.
func recordBinds(binds *[]desktopBind) listenFunc {
	return func(network, address string) (net.Listener, error) {
		l, err := net.Listen(network, address)
		bind := desktopBind{Address: address, Err: err}
		if err == nil {
			bind.Port = l.Addr().(*net.TCPAddr).Port
		}
		*binds = append(*binds, bind)
		return l, err
	}
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
