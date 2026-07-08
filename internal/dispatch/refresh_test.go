package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcadolph/yardmaster/internal/invsource"
)

func TestValidateBareSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	execFile := filepath.Join(dir, "dyn.sh")
	if err := os.WriteFile(execFile, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write exec file: %v", err)
	}
	plainFile := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(plainFile, []byte("[web]\nweb01\n"), 0o600); err != nil {
		t.Fatalf("write plain file: %v", err)
	}

	tests := []struct {
		Name string
		In   string
		Want error
	}{
		{Name: "empty", In: "", Want: invsource.ErrInvalidSource},                              // Test 0.
		{Name: "traversal relative", In: "../etc/passwd", Want: invsource.ErrInvalidSource},    // Test 1.
		{Name: "traversal absolute", In: dir + "/../../etc", Want: invsource.ErrInvalidSource}, // Test 2.
		{Name: "executable file", In: execFile, Want: invsource.ErrInvalidSource},              // Test 3.
		{Name: "plain inventory file", In: plainFile, Want: nil},                               // Test 4.
		{Name: "directory", In: dir, Want: nil},                                                // Test 5.
		{Name: "nonexistent", In: filepath.Join(dir, "ghost"), Want: nil},                      // Test 6.
	}
	for i, test := range tests {
		if err := validateBareSource(test.In); !errors.Is(err, test.Want) {
			t.Errorf("test %d (%s): validateBareSource(%q) error = %v, want %v",
				i, test.Name, test.In, err, test.Want)
		}
	}
}
