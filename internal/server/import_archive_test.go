package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/importer"
)

// archiveOf builds a zip holding the named members, for the archive refusals below.
func archiveOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestImportPassesAnArchiveRefusalThrough covers the sentence an archive reader writes for the
// person who built the archive. The command line prints it, and this route answers with it too,
// so a reader whose zip holds a bare config.xml, or a member no export writes, is told what is
// wrong with the archive rather than "could not read the export, check the format".
func TestImportPassesAnArchiveRefusalThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Format   string
		Body     []byte
		WantText string
	}{{ // Test 0: A Jenkins archive whose only config.xml has no directory naming it.
		Format: "jenkins", WantText: "naming its job",
		Body: archiveOf(t, map[string]string{"config.xml": "<project/>"}),
	}, { // Test 1: A Rundeck archive carrying an absolute path, which no export writes.
		Format: "rundeck", WantText: "absolute path",
		Body: archiveOf(t, map[string]string{"/etc/passwd": "root:x:0:0"}),
	}, { // Test 2: A body marked as UTF-16 that is not whole text.
		Format: "awx", WantText: "not whole text",
		Body: []byte("\xff\xfe{\x00}"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Format), func(t *testing.T) {
			t.Parallel()
			handler := importHandler(func() (importer.ApplyStores, bool) {
				return importer.ApplyStores{}, false
			}, zap.NewNop())
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/import/"+test.Format,
				bytes.NewReader(test.Body))
			req.SetPathValue("format", test.Format)
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			body := rec.Body.String()
			if !strings.Contains(body, test.WantText) {
				t.Errorf("the refusal does not carry the reader's own reason %q: %s",
					test.WantText, body)
			}
			if strings.Contains(body, "check the format") {
				t.Errorf("the refusal blames the format instead of saying what was wrong: %s", body)
			}
		})
	}
}
