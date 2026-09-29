package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

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

// newAccount builds a stored account whose password verifies at the cheapest bcrypt cost.
//
// The account is a real one, built the way the server builds it, with only the stored hash swapped.
// The cost user.New picks is deliberately slow and is pinned by the user package's own tests; here
// it is pure delay, and delay is what made the sign-in tests below unreliable. Each attempt in the
// throttle tests costs one comparison against this hash, and at the default cost eleven of them
// under the race detector on a loaded machine can run long enough for the limiter's one minute
// window to roll over mid-test. A cheap hash verifies exactly like a slow one.
func newAccount(t *testing.T, username, password string, role user.Role) *user.User {
	t.Helper()
	u, err := user.New(username, password, role)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	u.PasswordHash = string(hash)
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
//
// It drives the real endpoint, so it proves the wiring: that the handler consults the limiter with
// the address and username, and refuses before authenticating. Eleven attempts still have to land
// inside the one minute window, so this test is not free of the clock; the cheap hash above is what
// buys the margin. Measured under six concurrent copies of this suite with the race detector on,
// the body runs about twenty seconds against that window, where the default cost ran past two
// minutes and failed. The window arithmetic is pinned by the limiter tests at the end of this file,
// which choose how much time has passed rather than spending it.
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
// cannot reset their allowance by opening a new source port. It drives the endpoint, since the
// property is that the handler keys on what clientAddr returns rather than on the raw address.
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

// TestClientAddrStripsThePort pins the key derivation on its own, including the forms a request
// arrives with that a naive split would mangle.
func TestClientAddrStripsThePort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		RemoteAddr string
		WantKey    string
	}{{ // Test 0: An address with a port keys on the host alone.
		Name: "host and port", RemoteAddr: "10.0.0.9:1000", WantKey: "10.0.0.9",
	}, { // Test 1: Another port from the same host is the same client.
		Name: "another port from the same host", RemoteAddr: "10.0.0.9:65000", WantKey: "10.0.0.9",
	}, { // Test 2: A bracketed IPv6 address keys on the address without its brackets.
		Name: "ipv6 with a port", RemoteAddr: "[2001:db8::1]:443", WantKey: "2001:db8::1",
	}, { // Test 3: An address with no port at all is used whole rather than dropped, since dropping
		// it would key every such caller together.
		Name: "no port", RemoteAddr: "10.0.0.9", WantKey: "10.0.0.9",
	}, { // Test 4: An empty remote address stays empty rather than becoming a shared key by accident.
		Name: "empty", RemoteAddr: "", WantKey: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader("{}"))
			req.RemoteAddr = test.RemoteAddr
			if diff := cmp.Diff(test.WantKey, clientAddr(req), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d (%s): clientAddr() mismatch (-want +got):\n%s",
					testNum, test.Name, diff)
			}
		})
	}
}

// seedWindow gives a key an open window that started age ago holding count attempts, which is how
// these tests choose what the limiter's clock reads instead of waiting for it. Elapsed time is the
// only thing the limiter asks the clock for, so setting the start is equivalent to setting the now.
func seedWindow(l *loginLimiter, key string, age time.Duration, count int) {
	l.windows[key] = &loginWindow{start: time.Now().Add(-age), count: count}
}

// TestLoginLimiterWindow pins the fixed window arithmetic that decides when a sign-in is refused.
// Driving it here rather than through eleven real sign-ins is what keeps the answer independent of
// how long the machine takes: each case states how long ago the window opened.
//
// The near-boundary cases sit five seconds either side of the length rather than on it. A window is
// seeded and then consulted a few map operations later, so the elapsed time the limiter measures is
// the case's age plus that gap; five seconds of slack pins the comparison closely while leaving no
// room for a scheduling delay to change the answer.
func TestLoginLimiterWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Age       time.Duration
		Count     int
		Max       int
		WantAllow bool
		WantCount int
	}{{ // Test 0: A key nothing has touched opens a window and is allowed.
		Name: "untouched key", WantAllow: true, WantCount: 1,
	}, { // Test 1: The last attempt the window has room for is allowed.
		Name: "last attempt in the window", Age: time.Second, Count: loginWindowMax - 1,
		WantAllow: true, WantCount: loginWindowMax,
	}, { // Test 2: One attempt past the cap is refused, and it still counts against the window.
		Name: "one past the cap", Age: time.Second, Count: loginWindowMax,
		WantAllow: false, WantCount: loginWindowMax + 1,
	}, { // Test 3: A spent window just inside the length is still refused.
		Name: "spent window inside the length", Age: loginWindowLength - 5*time.Second,
		Count: loginWindowMax, WantAllow: false, WantCount: loginWindowMax + 1,
	}, { // Test 4: A window past the length is replaced, so the allowance returns after it lapses.
		Name: "window past the length", Age: loginWindowLength + 5*time.Second,
		Count: loginWindowMax, WantAllow: true, WantCount: 1,
	}, { // Test 5: A limiter with its own cap, the shape the webhook budget uses, honors that cap.
		Name: "own cap reached", Age: time.Second, Max: 3, Count: 3,
		WantAllow: false, WantCount: 4,
	}, { // Test 6: The same limiter allows the attempts below its cap.
		Name: "own cap not reached", Age: time.Second, Max: 3, Count: 2,
		WantAllow: true, WantCount: 3,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			limiter := &loginLimiter{windows: make(map[string]*loginWindow), max: test.Max}
			const key = "10.0.0.9\x00alice"
			if test.Count > 0 {
				seedWindow(limiter, key, test.Age, test.Count)
			}
			if got := limiter.allow(key); got != test.WantAllow {
				t.Errorf("test %d (%s): allow() = %v, want %v", testNum, test.Name, got, test.WantAllow)
			}
			if got := limiter.windows[key].count; got != test.WantCount {
				t.Errorf("test %d (%s): window count = %d, want %d",
					testNum, test.Name, got, test.WantCount)
			}
		})
	}
}

// TestLoginLimiterKeysAreIndependent pins that a spent window refuses its own key and nothing else.
// The key carries both the client and the username, so a shared key would let one guesser lock out
// an account they never named, or every account behind one address.
func TestLoginLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	limiter := &loginLimiter{windows: make(map[string]*loginWindow)}
	spentKey := "10.0.0.1\x00alice"
	seedWindow(limiter, spentKey, time.Second, loginWindowMax)
	if limiter.allow(spentKey) {
		t.Fatal("allow() on a spent window = true, want the key refused")
	}
	for _, key := range []string{"10.0.0.1\x00bob", "10.0.0.2\x00alice"} {
		if !limiter.allow(key) {
			t.Errorf("allow(%q) = false, want an untouched key unaffected by another's window", key)
		}
	}
}

// TestLoginLimiterSpentPeeks pins the half of the address budget that only failures pay into: spent
// reads the window without consuming an attempt, so a person signing in correctly never spends the
// allowance their neighbors behind the same address are sharing.
func TestLoginLimiterSpentPeeks(t *testing.T) {
	t.Parallel()
	limiter := &loginLimiter{windows: make(map[string]*loginWindow)}
	const addr = "10.0.0.1"
	if limiter.spent(addr, loginAddressMax) {
		t.Error("spent() on an untouched key = true, want a fresh address allowed")
	}
	seedWindow(limiter, addr, time.Second, loginAddressMax-1)
	if limiter.spent(addr, loginAddressMax) {
		t.Error("spent() one below the cap = true, want the last attempt allowed")
	}
	limiter.record(addr)
	if !limiter.spent(addr, loginAddressMax) {
		t.Error("spent() at the cap = false, want the address refused")
	}
	for range 3 {
		limiter.spent(addr, loginAddressMax)
	}
	if got := limiter.windows[addr].count; got != loginAddressMax {
		t.Errorf("window count after peeking = %d, want %d: spent() consumed attempts",
			got, loginAddressMax)
	}
	// A window past its length is not spent, so the budget returns rather than locking an address
	// out for good.
	seedWindow(limiter, addr, loginWindowLength+5*time.Second, loginAddressMax)
	if limiter.spent(addr, loginAddressMax) {
		t.Error("spent() on a lapsed window = true, want the address allowed again")
	}
}
