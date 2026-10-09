package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestPagesSayWhichFeaturesAreOff covers the marker the users and credentials pages carry naming
// the optional features the server has switched off, so the script asks for none of them. A page
// told nothing carries no marker and asks for every feature.
func TestPagesSayWhichFeaturesAreOff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantAttr string
		Off      []string
	}{{ // Test 0: Every feature that is off is named, in the order given.
		Off:      []string{"tokens", "credential-types", "federation"},
		WantAttr: ` data-features-off="tokens credential-types federation"`,
	}, { // Test 1: One feature off names only that one.
		Off: []string{"federation"}, WantAttr: ` data-features-off="federation"`,
	}, { // Test 2: Nothing off leaves the marker out.
		Off: nil, WantAttr: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(zap.NewNop(), nil, false, 0, false, false, false, "",
				WithFeaturesOff(test.Off...)).Handler()
			for _, path := range []string{"/ui/users", "/ui/credentials"} {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				body := rec.Body.String()
				if test.WantAttr == "" {
					if strings.Contains(body, "data-features-off") {
						t.Errorf("GET %s carries a features-off marker with nothing off", path)
					}
					continue
				}
				if !strings.Contains(body, test.WantAttr) {
					t.Errorf("GET %s does not carry %s", path, test.WantAttr)
				}
			}
		})
	}
}
