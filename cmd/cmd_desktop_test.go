package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	l2, err := desktopListener(dir)
	if err != nil {
		t.Fatalf("desktopListener() reuse error = %v", err)
	}
	if got := l2.Addr().(*net.TCPAddr).Port; got != first {
		t.Errorf("reused port = %d, want %d", got, first)
	}
	_ = l2.Close()

	// Test 2: A taken saved port falls back to a fresh one.
	blocker, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first))
	if err != nil {
		t.Fatalf("Listen() blocker error = %v", err)
	}
	defer func() { _ = blocker.Close() }()
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
