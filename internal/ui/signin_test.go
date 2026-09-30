package ui_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

// signInFlag is the attribute a page carries when a signed-out visitor has to sign in before it can
// read anything.
const signInFlag = `data-signin="required"`

// TestDataPagesSayWhenSignInIsRequired covers a signed-out visit to an install that authenticates.
//
// Every page asked the server for its data and was refused before the script sent the visitor to
// sign in, so the first page a stranger opened logged a 401 for each request it made. The page now
// says whether sign-in is required, so the script can go there first. Only the pages that load data
// carry the flag: the sign-in page itself must never send a visitor to sign in, and the docs and
// the not found page ask the server for nothing.
func TestDataPagesSayWhenSignInIsRequired(t *testing.T) {
	t.Parallel()
	exempt := map[string]bool{"/ui/login": true, "/ui/docs": true, "/ui/docs/guide": true}
	tests := []struct {
		Name     string
		Required bool
	}{{ // Test 0: An install that authenticates marks every page that loads data.
		Name: "sign-in required", Required: true,
	}, { // Test 1: An install that runs open marks nothing, so no visitor is sent away.
		Name: "open install", Required: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := ui.New(zap.NewNop(), testDocs, false, 5000, false, false, false, "",
				ui.WithSignInCheck(func() bool { return test.Required })).Handler()
			for _, path := range append(registeredRoutes(t), "/ui/no-such-page") {
				if strings.HasPrefix(path, "/ui/assets/") {
					continue
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				want := test.Required && !exempt[path] && path != "/ui/no-such-page"
				if got := strings.Contains(rec.Body.String(), signInFlag); got != want {
					t.Errorf("%s: GET %s carries %s = %v, want %v", test.Name, path, signInFlag, got, want)
				}
			}
		})
	}
}

// TestSignInIsNotRequiredWhenNobodySaid pins the default. A caller that never says whether sign-in
// is required gets pages that load as they always did, because the flag sends every signed-out
// visitor away and on an open install that is a loop between the page and the sign-in screen.
func TestSignInIsNotRequiredWhenNobodySaid(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 5000, false, false, false, "").Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if strings.Contains(rec.Body.String(), signInFlag) {
		t.Errorf("the overview carries %s with no sign-in check configured", signInFlag)
	}
}
