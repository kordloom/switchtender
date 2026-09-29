package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestBrowsingFilesWithoutCheckoutsExplainsItself covers a dead end found on the live demo.
//
// The Files button is offered on every project row, because browsing is a read and a read-only
// install still allows reads. An install that syncs no projects has no checkout to browse, and the
// refusal said "project files are not enabled": a sentence that reads as a feature somebody forgot
// to switch on. A visitor goes looking for the setting. There is none.
//
// The button cannot currently be hidden, because nothing tells the interface whether this server
// keeps checkouts. Until something does, the refusal has to carry the explanation.
func TestBrowsingFilesWithoutCheckoutsExplainsItself(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/projects/proj_1/files", nil)
	// No store and no syncer, which is the shape of an install that browses nothing.
	if _, ok := projectBrowseTarget(rec, req, nil, nil, nil, zap.NewNop()); ok {
		t.Fatal("browsing was allowed with no checkout to browse")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "not enabled") {
		t.Errorf("the refusal still reads as a switch somebody left off: %s", body)
	}
	// It has to say what is true, so the reader stops rather than hunting for a setting.
	if !strings.Contains(body, "keeps no project checkouts") {
		t.Errorf("the refusal does not say why there is nothing to browse: %s", body)
	}
}
