package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// TestACorrectPasswordSurvivesAFloodFromTheSameAddress is the defect this file exists for.
//
// The address budget used to be checked before the credential, which read as a brake on hashing
// work and was a lockout in practice. Behind a reverse proxy every client shares one remote address,
// and the limiter deliberately does not trust forwarding headers, so one stranger spending the
// budget on wrong guesses refused everyone else, correct password and all. That takes out the
// approval queue, which is the one action this product exists for, and it costs an unauthenticated
// attacker about thirty requests a minute.
//
// A correct credential must always win. The budget still applies to failures, so the sweep the
// sibling test covers is still cut off.
func TestACorrectPasswordSurvivesAFloodFromTheSameAddress(t *testing.T) {
	t.Parallel()
	const addr = "203.0.113.9:5555"
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

	post := func(username, password string) *httptest.ResponseRecorder {
		body := `{"username":"` + username + `","password":"` + password + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec
	}

	// Spend the address budget the way an attacker does, on usernames nobody owns, so the
	// per-username brake is never what refuses anything.
	for i := range loginAddressMax + 1 {
		post("nobody"+strconv.Itoa(i), "wrong")
	}

	// The person who actually has the password, from the same address a moment later.
	rec := post("casey", "correct-horse")
	if rec.Code != http.StatusOK {
		t.Fatalf("a correct sign-in from a flooded address returned %d, want 200: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("sign-in response is not JSON: %v", err)
	}
	if out["token"] == nil || out["token"] == "" {
		t.Errorf("sign-in returned 200 with no token: %v", out)
	}
}

// TestAWrongPasswordStillPaysTheAddressBudget pins the other half. Fixing the lockout must not
// remove the brake: once the budget is spent, a failing attempt from that address is refused with
// 429 rather than the ordinary 401, so a stuffing sweep still stops.
func TestAWrongPasswordStillPaysTheAddressBudget(t *testing.T) {
	t.Parallel()
	const addr = "203.0.113.11:5555"
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

	post := func(username string) int {
		body := `{"username":"` + username + `","password":"wrong"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Code
	}

	var refused int
	for i := range loginAddressMax * 3 {
		if post("nobody"+strconv.Itoa(i)) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("no failing attempt was ever refused, so the address brake is gone: a stuffing " +
			"sweep now runs at full speed")
	}
}
