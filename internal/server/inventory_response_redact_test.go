package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/user"

	"go.uber.org/zap"
)

// TestInventoryUpdateResponseRedactsForNonAdmin proves the update handler's response never hands a
// non-admin the inline host-variable secrets the list path hides from them. A non-admin manager is
// served a redacted inventory, edits its name, and submits the redacted content back; the handler
// restores the real secret to storage so it is not destroyed, then must redact it again on the way
// out. Returning the restored plaintext leaked exactly the credential the read path was built to
// withhold. An admin still gets the full content, since an admin already holds every secret.
func TestInventoryUpdateResponseRedactsForNonAdmin(t *testing.T) {
	t.Parallel()
	const secret = "Hunter2!"
	stored := "[web]\nweb1 ansible_host=10.0.0.5 ansible_password=" + secret + "\n"
	redacted := inventory.Redact(stored)

	put := func(role user.Role) *httptest.ResponseRecorder {
		store := inventory.NewMemStore()
		if err := store.Save(context.Background(), &inventory.Inventory{
			ID: "inv_1", Name: "fleet", Content: stored, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		h := updateInventoryHandler(store, &authorizer{}, nil, zap.NewNop())
		// The manager submits the redacted content it was shown, only renaming the inventory.
		body := `{"name":"fleet-renamed","content":` + jsonString(redacted) + `}`
		req := httptest.NewRequest(http.MethodPut, "/v1/inventories/inv_1", strings.NewReader(body))
		req.SetPathValue("id", "inv_1")
		req = req.WithContext(context.WithValue(req.Context(), actorKey{}, Actor{UserID: "u", Role: role}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// The stored secret must survive regardless of who edited it.
		got, err := store.Get(context.Background(), "inv_1")
		if err != nil || !strings.Contains(got.Content, secret) {
			t.Fatalf("the stored secret was destroyed by the edit: %q (err %v)", got.Content, err)
		}
		return rec
	}

	if rec := put(user.RoleOperator); strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the update response handed a non-admin the plaintext secret it should not see:\n%s",
			rec.Body.String())
	}
	if rec := put(user.RoleAdmin); !strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the update response redacted the secret from an admin, who already holds it:\n%s",
			rec.Body.String())
	}
}

// jsonString renders s as a JSON string literal for the request body.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
