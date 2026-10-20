package runfiles

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestFreeBytesReadsTheFilesystem pins that a directory reports some free space and a missing path
// is ErrFreeSpace rather than zero, which the floor would read as a full disk.
func TestFreeBytesReadsTheFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	free, err := FreeBytes(dir)
	if err != nil {
		t.Fatalf("FreeBytes(%s) error = %v", dir, err)
	}
	if free == 0 {
		t.Errorf("FreeBytes(%s) = 0, want the space the test is writing to", dir)
	}
	if _, err := FreeBytes(filepath.Join(dir, "missing", "dir")); !errors.Is(err, ErrFreeSpace) {
		t.Errorf("FreeBytes(missing) error = %v, want ErrFreeSpace", err)
	}
}
