package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// newAccountServer returns a handler backed by the given accounts, plus the stores so a test can
// inspect what a request changed.
func newAccountServer(t *testing.T, accounts ...*user.User) (http.Handler, user.Store, auth.Store) {
	t.Helper()
	users := user.NewMemStore()
	for _, u := range accounts {
		if err := users.Save(context.Background(), u); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	tokens := auth.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithUsers(users), WithTokens(tokens)).Handler()
	return handler, users, tokens
}

// newAccount builds a stored account with a hashed password.
func newAccount(t *testing.T, username, password string, role user.Role) *user.User {
	t.Helper()
	u, err := user.New(username, password, role)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	return u
}

// login posts a sign-in attempt from a fixed client address, so the limiter keys stay stable
// across a test's attempts.
func login(handler http.Handler, addr, username, password string) *httptest.ResponseRecorder {
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// bearerFor signs in and returns the session token, so a test can make the authenticated requests a
// real administrator makes. The API authenticates whenever an account exists, so an admin-only test
// cannot reach a mutating route without one.
func bearerFor(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()
	rec := login(handler, "10.0.0.1:1234", username, password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login for %s = %d, want 200 (%s)", username, rec.Code, rec.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if body.Token == "" {
		t.Fatal("login returned no token")
	}
	return body.Token
}

// TestLoginRateLimit verifies repeated bad passwords are throttled per client and username, that
// the limit is keyed so one victim's lockout cannot lock out another account or client, and that
// throttling outranks correct credentials so a guesser cannot slip through on attempt eleven.
func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	handler, _, _ := newAccountServer(t,
		newAccount(t, "alice", "correct-horse", user.RoleAdmin),
		newAccount(t, "bob", "another-secret", user.RoleOperator))

	// The window allows ten attempts; the eleventh is refused.
	for i := range loginWindowMax {
		if rec := login(handler, "10.0.0.1:1234", "alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := login(handler, "10.0.0.1:1234", "alice", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429", loginWindowMax+1, rec.Code)
	}

	// Throttling wins over correct credentials, so exhausting guesses cannot be followed by a
	// successful sign-in inside the same window.
	if rec := login(handler, "10.0.0.1:1234", "alice", "correct-horse"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while throttled = %d, want 429", rec.Code)
	}

	// A different account from the same client is unaffected, so one user cannot lock out another.
	if rec := login(handler, "10.0.0.1:1234", "bob", "another-secret"); rec.Code != http.StatusOK {
		t.Errorf("other account from the same client = %d, want 200", rec.Code)
	}

	// The same account from a different client is unaffected, so a shared username cannot be
	// locked out globally by one attacker.
	if rec := login(handler, "10.0.0.2:1234", "alice", "correct-horse"); rec.Code != http.StatusOK {
		t.Errorf("same account from another client = %d, want 200", rec.Code)
	}
}

// TestLoginRejectsBadCredentials verifies an unknown user and a wrong password are refused
// identically, so the response cannot be used to enumerate accounts.
func TestLoginRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	handler, _, _ := newAccountServer(t, newAccount(t, "alice", "correct-horse", user.RoleAdmin))

	unknown := login(handler, "10.0.0.3:1", "nobody", "whatever")
	wrong := login(handler, "10.0.0.4:1", "alice", "wrong")
	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("unknown = %d, wrong password = %d, want both 401", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Errorf("responses differ, which enumerates accounts:\n unknown: %s\n wrong:   %s",
			unknown.Body.String(), wrong.Body.String())
	}
}

// TestLoginMintsSessionToken verifies a good sign-in returns a token that is actually stored, so
// the session works on the next request.
func TestLoginMintsSessionToken(t *testing.T) {
	t.Parallel()
	handler, _, tokens := newAccountServer(t, newAccount(t, "alice", "correct-horse", user.RoleAdmin))

	rec := login(handler, "10.0.0.5:1", "alice", "correct-horse")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "token") {
		t.Errorf("login body carries no token: %s", rec.Body.String())
	}
	list, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(list))
	}
	if list[0].ExpiresAt == nil {
		t.Error("session token has no expiry, so it would outlive the session")
	}
}

// TestLastAdminIsProtected verifies the final admin account cannot be deleted or demoted, since
// either would lock everyone out of administration permanently.
func TestLastAdminIsProtected(t *testing.T) {
	t.Parallel()
	admin := newAccount(t, "alice", "correct-horse", user.RoleAdmin)
	viewer := newAccount(t, "reader", "another-secret", user.RoleViewer)
	handler, users, _ := newAccountServer(t, admin, viewer)
	bearer := bearerFor(t, handler, "alice", "correct-horse")

	rec := httptest.NewRecorder()
	del := httptest.NewRequest(http.MethodDelete, "/v1/users/"+admin.ID, nil)
	del.Header.Set("Authorization", "Bearer "+bearer)
	handler.ServeHTTP(rec, del)
	if rec.Code != http.StatusConflict {
		t.Errorf("deleting the last admin = %d, want 409", rec.Code)
	}

	rec = httptest.NewRecorder()
	demote := httptest.NewRequest(http.MethodPut, "/v1/users/"+admin.ID,
		strings.NewReader(`{"username":"alice","role":"viewer"}`))
	demote.Header.Set("Authorization", "Bearer "+bearer)
	handler.ServeHTTP(rec, demote)
	if rec.Code != http.StatusConflict {
		t.Errorf("demoting the last admin = %d, want 409", rec.Code)
	}

	// The account survived both attempts and is still an admin.
	after, err := users.Get(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if after.Role != user.RoleAdmin {
		t.Errorf("role = %q, want it unchanged as admin", after.Role)
	}

	// With a second admin present, the first may be removed.
	second := newAccount(t, "morgan", "third-secret", user.RoleAdmin)
	if err := users.Save(context.Background(), second); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	rec = httptest.NewRecorder()
	delSecond := httptest.NewRequest(http.MethodDelete, "/v1/users/"+admin.ID, nil)
	delSecond.Header.Set("Authorization", "Bearer "+bearer)
	handler.ServeHTTP(rec, delSecond)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("deleting an admin with another present = %d, want success", rec.Code)
	}
}

// TestClientAddrKeying verifies the limiter key uses the host without its port, so an attacker
// cannot reset their allowance by opening a new source port.
func TestClientAddrKeying(t *testing.T) {
	t.Parallel()
	handler, _, _ := newAccountServer(t, newAccount(t, "alice", "correct-horse", user.RoleAdmin))

	for i := range loginWindowMax {
		if rec := login(handler, fmt.Sprintf("10.0.0.9:%d", 1000+i), "alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	// A fresh port from the same host is the same client and stays throttled.
	if rec := login(handler, "10.0.0.9:65000", "alice", "wrong"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("new source port = %d, want 429 since the host is the same client", rec.Code)
	}
}
