package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// withTrustedProxy points the client resolver at a proxy network for one test and restores it after.
func withTrustedProxy(t *testing.T, cidr string) {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	oldNets, oldHeader := trustedProxies, clientIPHeader
	t.Cleanup(func() { trustedProxies, clientIPHeader = oldNets, oldHeader })
	SetTrustedProxies([]*net.IPNet{n})
	SetClientIPHeader("")
}

// floodUsers returns a login handler over one real account, plus a poster that sends an attempt from
// a given peer address carrying a given forwarded client address.
func floodUsers(t *testing.T) func(peer, forwarded, username, password string) int {
	t.Helper()
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
	return func(peer, forwarded, username, password string) int {
		body := `{"username":"` + username + `","password":"` + password + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
		r.RemoteAddr = peer
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Code
	}
}

// TestAFloodBehindAProxyDoesNotLockOutOtherClients is the defect this file exists for.
//
// The sign-in brake keys on the client address and is checked before any password hashing, which is
// what makes it a brake on the work rather than only on the outcome. Keyed on the raw peer address
// that is also a lockout: behind a reverse proxy every client arrives from the proxy's address, so
// one stranger's failed guesses spent the budget for everybody and refused the whole install,
// correct password included. That takes out the approval queue, which is the one action this
// product exists for, and it costs an unauthenticated attacker about thirty requests a minute.
//
// Resolving the real client behind a proxy the operator explicitly trusted fixes it at the key
// rather than by weakening the brake.
func TestAFloodBehindAProxyDoesNotLockOutOtherClients(t *testing.T) {
	// Not parallel: the trusted-proxy configuration is a package global.
	withTrustedProxy(t, "10.0.0.0/8")
	post := floodUsers(t)
	const proxy = "10.0.0.9:443"

	// One client behind the proxy burns its whole budget on wrong guesses.
	for i := range loginAddressMax + 5 {
		post(proxy, "198.51.100.7", "nobody"+strconv.Itoa(i), "wrong")
	}
	if got := post(proxy, "198.51.100.7", "casey", "correct-horse"); got != http.StatusTooManyRequests {
		t.Errorf("the flooding client itself got %d, want 429: its own budget must still be spent", got)
	}

	// A different client behind the same proxy is untouched, which is the whole point.
	if got := post(proxy, "203.0.113.42", "casey", "correct-horse"); got != http.StatusOK {
		t.Fatalf("an unrelated client behind the same proxy got %d, want 200: one stranger's failed "+
			"guesses have locked the install out of its own approval queue", got)
	}
}

// TestAForgedForwardedHeaderIsIgnoredWithoutATrustedProxy is the other half, and the reason the
// header is not simply believed. If any caller could set its own key, the brake would be free to
// evade: a flooder would send a fresh forwarded address on every request and never spend a budget.
func TestAForgedForwardedHeaderIsIgnoredWithoutATrustedProxy(t *testing.T) {
	// Not parallel: the trusted-proxy configuration is a package global.
	oldNets, oldHeader := trustedProxies, clientIPHeader
	t.Cleanup(func() { trustedProxies, clientIPHeader = oldNets, oldHeader })
	SetTrustedProxies(nil)
	post := floodUsers(t)

	// No proxy is trusted, so a stranger varying the header must not buy a fresh budget each time.
	var refused int
	for i := range loginAddressMax * 3 {
		forged := "198.51.100." + strconv.Itoa(i%250+1)
		if post("203.0.113.99:5555", forged, "nobody"+strconv.Itoa(i), "wrong") == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("a flooder varying X-Forwarded-For was never refused: the header is being believed " +
			"from an untrusted peer, so the address brake can be evaded by anyone")
	}
}

// TestATrustedProxyCannotBeImpersonatedByANeighbor pins that trust is scoped to the network the
// operator named. A caller outside it keeps its own address as the key however it labels itself.
func TestATrustedProxyCannotBeImpersonatedByANeighbor(t *testing.T) {
	// Not parallel: the trusted-proxy configuration is a package global.
	withTrustedProxy(t, "10.0.0.0/8")
	post := floodUsers(t)

	// This caller is not in 10.0.0.0/8, so its forwarded header must be ignored and its own address
	// used. Varying the header therefore buys it nothing.
	var refused int
	for i := range loginAddressMax * 3 {
		forged := "198.51.100." + strconv.Itoa(i%250+1)
		if post("203.0.113.50:5555", forged, "nobody"+strconv.Itoa(i), "wrong") == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("a caller outside the trusted network evaded the brake by setting a header: trust " +
			"is not scoped to the configured proxy network")
	}
}
