package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
)

// inventoryOfSize returns INI content exactly n bytes long: one group and host, then comment lines
// made of filler, so the content parses and its size is all filler.
func inventoryOfSize(n int, filler byte) string {
	head := "[web]\nweb01\n"
	var b strings.Builder
	b.Grow(n)
	b.WriteString(head)
	line := "#" + strings.Repeat(string(filler), 1022) + "\n"
	for b.Len()+len(line) <= n {
		b.WriteString(line)
	}
	if rest := n - b.Len(); rest > 0 {
		b.WriteString("#" + strings.Repeat(string(filler), rest-1))
	}
	return b.String()
}

// TestInventoryLimitOnSave pins the one content limit a stored inventory has: content of exactly
// inventory.MaxContentBytes is stored and one byte more is refused naming both sizes. The write
// route's body limit is derived from the content limit, so content within it always writes back,
// including content whose JSON encoding doubles it, such as quotes and newlines, and content of `<`
// written by an encoder that does not escape HTML, as the web page's does.
func TestInventoryLimitOnSave(t *testing.T) {
	t.Parallel()
	limit := inventory.MaxContentBytes
	tests := []struct {
		Content    string
		EscapeHTML bool
		WantStatus int
		WantMsg    []string
	}{{ // Test 0: Exactly the limit is stored.
		Content: inventoryOfSize(limit, 'x'), EscapeHTML: true, WantStatus: http.StatusCreated,
	}, { // Test 1: One byte past the limit is refused with both numbers.
		Content: inventoryOfSize(limit+1, 'x'), EscapeHTML: true, WantStatus: http.StatusBadRequest,
		WantMsg: []string{strconv.Itoa(limit + 1), strconv.Itoa(limit), "16 MiB"},
	}, { // Test 2: The limit in quotes, each two bytes in JSON, writes back through the route.
		Content: inventoryOfSize(limit, '"'), EscapeHTML: true, WantStatus: http.StatusCreated,
	}, { // Test 3: The limit in `<`, written without HTML escaping, writes back through the route.
		Content: inventoryOfSize(limit, '<'), EscapeHTML: false, WantStatus: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			store := inventory.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithInventories(store)).Handler()
			var body bytes.Buffer
			enc := json.NewEncoder(&body)
			enc.SetEscapeHTML(test.EscapeHTML)
			if err := enc.Encode(map[string]string{"name": "big", "content": test.Content}); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/inventories", &body))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d: %.300s", rec.Code, test.WantStatus, rec.Body.String())
			}
			for _, want := range test.WantMsg {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("refusal %s does not hold %q", rec.Body.String(), want)
				}
			}
			list, err := store.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stored := len(list) == 1; stored != (test.WantStatus == http.StatusCreated) {
				t.Errorf("stored = %v, want %v", stored, test.WantStatus == http.StatusCreated)
			}
		})
	}
}
