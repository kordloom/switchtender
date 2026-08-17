package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// TestSignInFloodIsBoundedPerAddress covers a limiter that counted the wrong thing. The window key
// included the username, so ten attempts were allowed per username rather than per client: a stranger
// varying the username on every request got unbounded attempts from one address, each one a full
// password hash comparison. That is both a credential-stuffing sweep across every account at full
// speed and a way to spend the server's CPU with no credential at all.
func TestSignInFloodIsBoundedPerAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	u, err := user.New("casey", "correct-horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, u); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	handler := loginHandler(users, auth.NewMemStore(), nil, zap.NewNop())

	attempt := func(username string) int {
		body := `{"username":"` + username + `","password":"wrong"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.7:5555"
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Code
	}

	// One address, a different username every time: the shape of a stuffing sweep. It has to be cut
	// off well before it has walked a password list.
	var refused int
	for i := 0; i < 200; i++ {
		if attempt("user"+strconv.Itoa(i)) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("200 sign-in attempts from one address, each with a fresh username, were all " +
			"accepted: the limiter counts per username, so varying it buys unlimited attempts")
	}
	if refused < 150 {
		t.Errorf("only %d of 200 varied-username attempts were refused, want the address cut off "+
			"early", refused)
	}

	// A different address is unaffected, so one attacker cannot lock everyone else out.
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login",
		strings.NewReader(`{"username":"casey","password":"correct-horse"}`))
	r.RemoteAddr = "198.51.100.4:5555"
	rec := httptest.NewRecorder()
	handler(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("sign-in from another address = %d, want 200: one flooder locked out the install",
			rec.Code)
	}
}
