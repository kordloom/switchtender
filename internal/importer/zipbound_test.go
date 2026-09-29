package importer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestJenkinsZipRefusesTooManyDefinitionsInTotal covers the ceiling on everything read out of one
// archive. Every member can sit under the per-entry ceiling while the archive as a whole is far past
// what should be held in memory, so the running total has to refuse on its own. Only the per-entry
// ceiling was covered, so deleting the total left every test green.
func TestJenkinsZipRefusesTooManyDefinitionsInTotal(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("x", maxJenkinsConfigSize)
	entries := map[string]string{}
	// Enough members that the total passes the ceiling while no single one reaches it.
	for i := range maxJenkinsTotalSize/maxJenkinsConfigSize + 1 {
		entries[fmt.Sprintf("jobs/job%02d/config.xml", i)] = body
	}
	_, err := JenkinsBundleFromZip(buildZip(t, entries))
	if err == nil {
		t.Fatal("JenkinsBundleFromZip() error = nil, want the total ceiling to refuse the archive")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Errorf("error = %v, want it to say the definitions exceeded what is read", err)
	}
}

// TestJenkinsZipRefusesTooManyEntries covers the ceiling on how many members are examined at all. It
// is the cheapest archive to make hostile: a zip of empty entries costs almost nothing to build and
// little to send, so the count is refused before any member is opened. Nothing covered it, so the
// ceiling could be deleted with every test still green.
func TestJenkinsZipRefusesTooManyEntries(t *testing.T) {
	t.Parallel()
	// The archive built below holds one member more than the ceiling allows, so it tracks whatever the
	// ceiling is set to. That alone would still pass if the ceiling were loosened to a number no upload
	// could reach, which is the same as having none, so the range it may sit in is pinned too.
	if maxJenkinsZipEntries < 1000 || maxJenkinsZipEntries > 100000 {
		t.Fatalf("maxJenkinsZipEntries = %d, want a ceiling a hostile upload can actually reach",
			maxJenkinsZipEntries)
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := range maxJenkinsZipEntries + 1 {
		if _, err := w.Create(fmt.Sprintf("jobs/j%d/notes.txt", i)); err != nil {
			t.Fatalf("create zip entry %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	_, err := JenkinsBundleFromZip(buf.Bytes())
	if err == nil {
		t.Fatal("JenkinsBundleFromZip() error = nil, want the entry-count ceiling to refuse it")
	}
	if !strings.Contains(err.Error(), "more than the") {
		t.Errorf("error = %v, want it to say the archive holds more entries than are read", err)
	}
}
