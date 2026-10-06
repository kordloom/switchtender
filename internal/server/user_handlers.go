package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// sessionTokenTTL is how long a browser session token stays valid.
const sessionTokenTTL = 30 * 24 * time.Hour

// loginRequest is the JSON body accepted by POST /auth/login.
type loginRequest struct {
	// Username is the account name.
	Username string `json:"username"`
	// Password is the account password, never logged.
	Password string `json:"password"`
}

// loginResponse returns the minted session token and the account's role.
type loginResponse struct {
	// Token authenticates subsequent requests.
	Token string `json:"token"`
	// Username echoes the account.
	Username string `json:"username"`
	// Role is the account's permission level.
	Role user.Role `json:"role"`
}

// userProfileRequest is the optional profile an account may carry, shared by create and update. The
// fields are personal data: they are stored, returned only to an admin, and never logged.
type userProfileRequest struct {
	// FullName is the person's name.
	FullName string `json:"full_name"`
	// Email is the address to reach the account.
	Email string `json:"email"`
	// Phone is a contact number for the account.
	Phone string `json:"phone"`
	// Title is what the person does. It carries no permission; Role decides that.
	Title string `json:"title"`
	// Links are http or https addresses that say more about the account.
	Links []string `json:"links"`
	// Notes is free text about the account.
	Notes string `json:"notes"`
}

// applyTo copies the profile onto a user and normalizes it, so the same validation runs on create and
// on update.
func (p userProfileRequest) applyTo(u *user.User) error {
	u.FullName = p.FullName
	u.Email = p.Email
	u.Phone = p.Phone
	u.Title = p.Title
	u.Links = p.Links
	u.Notes = p.Notes
	return u.NormalizeProfile()
}

// createUserRequest is the JSON body accepted by POST /users.
type createUserRequest struct {
	// Username is the unique sign in name. Required.
	Username string `json:"username"`
	// Password is the initial password. Required, never logged.
	Password string `json:"password"`
	// Role is admin, operator, or viewer. Required.
	Role user.Role `json:"role"`
	// userProfileRequest carries the optional profile fields.
	userProfileRequest
}

// listUsersResponse wraps the user list, password hashes excluded by the model's json tags.
type listUsersResponse struct {
	// Users is the ordered list.
	Users []*user.User `json:"users"`
	// Count is the number returned.
	Count int `json:"count"`
	// Total is how many rows exist before the response cap, so a caller shown a prefix knows it is
	// one. Equal to Count for every ordinary install.
	Total int `json:"total"`
}

// loginWindowLength and loginWindowMax bound sign-in attempts per client and username to a fixed
// window, the brake on credential stuffing against the unauthenticated login endpoint.
const (
	loginWindowLength = time.Minute
	loginWindowMax    = 10
	// loginAddressMax bounds failed sign-ins from one client address, whatever usernames they name.
	// The per-username window above cannot do this: its key includes the username, so a caller who
	// varies it gets a fresh budget every request, and each request costs a full password hash. That
	// is a credential-stuffing sweep across every account at full speed, and a way to spend the
	// server's processor with no credential at all.
	//
	// Only failures count against it. A person signing in successfully never touches this budget, so
	// a whole office behind one address is unaffected however many of them sign in at once, while an
	// attacker guessing wrong is cut off after thirty tries a minute.
	loginAddressMax = 30
)

// loginLimiter is a fixed-window sign-in counter keyed by client address and username.
type loginLimiter struct {
	// mu guards windows.
	mu sync.Mutex
	// windows tracks the open window per key.
	windows map[string]*loginWindow
	// max is how many attempts a window allows. Zero means loginWindowMax, so a sign-in limiter
	// needs no configuration and a caller with a different shape of traffic can state its own.
	max int
	// now reads the clock. Nil means time.Now. A test sets it so a window cannot roll over midway
	// through a burst, which is the difference between asserting on the limiter and asserting on how
	// fast the machine happened to be.
	now func() time.Time
}

// clock returns the limiter's time source, defaulting to the real one.
func (l *loginLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// loginWindow is one key's open window.
type loginWindow struct {
	// start is when the window opened.
	start time.Time
	// count is how many attempts landed in the window.
	count int
}

// allow consumes one attempt for the key, reporting false when the window is spent. Expired
// windows are pruned once the map grows past a bound, so an address sweep cannot grow it forever.
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if len(l.windows) > 4096 {
		for k, w := range l.windows {
			if now.Sub(w.start) > loginWindowLength {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) > loginWindowLength {
		l.windows[key] = &loginWindow{start: now, count: 1}
		return true
	}
	w.count++
	limit := l.max
	if limit <= 0 {
		limit = loginWindowMax
	}
	return w.count <= limit
}

// spent reports whether the key's window is already used up, without consuming an attempt. It is the
// peek half of a budget that only failures pay into.
func (l *loginLimiter) spent(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[key]
	if !ok || l.clock().Sub(w.start) > loginWindowLength {
		return false
	}
	return w.count >= max
}

// record consumes one attempt for the key, opening a window when none is current.
func (l *loginLimiter) record(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if len(l.windows) > 4096 {
		for k, w := range l.windows {
			if now.Sub(w.start) > loginWindowLength {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) > loginWindowLength {
		l.windows[key] = &loginWindow{start: now, count: 1}
		return
	}
	w.count++
}

// trustedProxies holds the networks whose forwarding headers this server believes, set by the
// operator with --trusted-proxy. Empty means believe nobody, which is the default.
var trustedProxies []*net.IPNet

// SetTrustedProxies configures which peers may set a client IP header. It is called once at startup.
func SetTrustedProxies(nets []*net.IPNet) { trustedProxies = nets }

// clientIPHeader names the header carrying the real client address, set by the operator with
// --client-ip-header. Empty means use the leftmost X-Forwarded-For entry.
var clientIPHeader string

// SetClientIPHeader configures which header carries the client address behind a trusted proxy.
func SetClientIPHeader(name string) { clientIPHeader = name }

// clientAddr returns the request's client host without the port, the stable half of the limiter key.
//
// The remote address is used as seen unless the immediate peer is a proxy the operator explicitly
// trusted, because a forwarding header from anyone else is a value a stranger chooses and would let
// them spend or evade any budget keyed on it. Trusting a named proxy is not a loosening: without it
// every client behind that proxy shares one key, so one stranger's failed guesses spend the budget
// for everybody, and the sign-in endpoint stops answering for the whole install.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(trustedProxies) == 0 {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !fromTrustedProxy(peer) {
		return host
	}
	if fwd := forwardedClient(r); fwd != "" {
		return fwd
	}
	return host
}

// fromTrustedProxy reports whether the immediate peer sits in a network the operator trusts.
func fromTrustedProxy(peer net.IP) bool {
	return ipInAny(peer, trustedProxies)
}

// ipInAny reports whether ip sits inside any of the given networks.
func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedClient reads the client address a trusted proxy forwarded, using the operator's
// configured header and trusted networks. The globals it reads are set once at boot and never
// after, which is what lets every request read them without a lock.
func forwardedClient(r *http.Request) string {
	return forwardedClientIn(r, clientIPHeader, trustedProxies)
}

// forwardedClientIn is the parsing itself, free of package state so it can be held to its contract
// directly: the operator's named header wins, and otherwise X-Forwarded-For is read from the right.
//
// The leftmost X-Forwarded-For entry is written by the original client, so trusting it let a caller
// put a fresh address there on every request and mint an unbounded set of limiter keys, walking
// straight around the per-client rate limit the header exists to key. The real client is the last
// entry not written by a proxy this install trusts: walking from the right, past the trusted hops,
// the first untrusted address is the furthest one this install can actually vouch for.
func forwardedClientIn(r *http.Request, header string, proxies []*net.IPNet) string {
	if header != "" {
		if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				return ip.String()
			}
		}
		return ""
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return ""
	}
	entries := strings.Split(xff, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(entries[i]))
		if ip == nil {
			// A malformed entry breaks the chain of trust: everything left of it is unverifiable,
			// so the closest trusted address is as far as this install can honestly reach.
			return ""
		}
		if !ipInAny(ip, proxies) {
			return ip.String()
		}
	}
	return ""
}

// The keys sign-in budgets are kept under in the store's shared allowances.
const (
	// loginAddressBudget prefixes the per-address failed sign-in budget.
	loginAddressBudget = "login-address:"
	// loginAttemptBudget prefixes the per-address and username attempt budget. The username stands in
	// the key as its digest, so a name of any length or content makes a key the store can index.
	loginAttemptBudget = "login-attempt:"
)

// loginBudgets counts sign-in attempts: in the store's shared allowances when it keeps them, so
// every replica spends from one budget, and in this process otherwise.
//
// A budget kept in each process is a budget per process. Behind a load balancer an attacker got the
// whole allowance again from every replica, and again from each one after it restarted, so ten
// replicas let a stuffing run try ten times the guesses the limit promises.
type loginBudgets struct {
	// shared is the store's allowances, nil when the store keeps none.
	shared run.Budgets
	// clock reads the store's time, which the shared windows are measured on, so replicas agree on
	// when one closes.
	clock run.Store
	// attempts is this process's per address and username budget, used when shared is nil.
	attempts *loginLimiter
	// addresses is this process's per-address failure budget, used when shared is nil. It is kept
	// apart from attempts so its keys cannot collide with theirs and its larger cap applies to
	// nothing else.
	addresses *loginLimiter
}

// newLoginBudgets returns the sign-in budgets for store, shared when it keeps allowances. now is
// the in-process limiters' clock, nil for the real one.
func newLoginBudgets(store run.Store, now func() time.Time) *loginBudgets {
	b := &loginBudgets{
		attempts:  &loginLimiter{windows: make(map[string]*loginWindow), now: now},
		addresses: &loginLimiter{windows: make(map[string]*loginWindow), now: now},
	}
	if shared, ok := store.(run.Budgets); ok {
		b.shared, b.clock = shared, store
	}
	return b
}

// addressSpent reports whether addr has used up its failed sign-in budget, without spending any of
// it.
func (b *loginBudgets) addressSpent(ctx context.Context, addr string) (bool, error) {
	if b.shared == nil {
		return b.addresses.spent(addr, loginAddressMax), nil
	}
	now, err := b.clock.Now(ctx)
	if err != nil {
		return false, err
	}
	spent, err := b.shared.BudgetSpent(ctx, loginAddressBudget+addr, now)
	return spent >= loginAddressMax, err
}

// allowAttempt spends one attempt at username from addr and reports whether its window allows it.
func (b *loginBudgets) allowAttempt(ctx context.Context, addr, username string) (bool, error) {
	if b.shared == nil {
		return b.attempts.allow(addr + "\x00" + username), nil
	}
	now, err := b.clock.Now(ctx)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256([]byte(username))
	spent, err := b.shared.SpendBudget(ctx, loginAttemptBudget+addr+":"+hex.EncodeToString(sum[:]),
		loginWindowLength, now)
	return spent <= loginWindowMax, err
}

// recordFailure pays one failed sign-in into addr's budget.
func (b *loginBudgets) recordFailure(ctx context.Context, addr string) error {
	if b.shared == nil {
		b.addresses.record(addr)
		return nil
	}
	now, err := b.clock.Now(ctx)
	if err != nil {
		return err
	}
	_, err = b.shared.SpendBudget(ctx, loginAddressBudget+addr, loginWindowLength, now)
	return err
}

// loginHandler authenticates a username and password and mints a session token owned by the user.
// Attempts are rate limited per client and username so stolen password lists cannot be replayed at
// full speed, and are counted in store's shared budgets when it keeps them.
func loginHandler(users user.Store, tokens auth.Store, ldap *LDAPAuth, store run.Store,
	log *zap.Logger) http.HandlerFunc {
	return budgetedLoginHandler(users, tokens, ldap, log, newLoginBudgets(store, nil))
}

// loginHandlerWithClock is loginHandler with budgets this process keeps on a stated clock. A test
// passes a frozen one so a burst cannot straddle a window boundary and fail for how fast the
// machine was rather than for anything about the limiter. The clock is per handler rather than a
// package variable, because a variable a test writes while a parallel handler reads it is a data
// race, and the race detector finds it.
func loginHandlerWithClock(users user.Store, tokens auth.Store, ldap *LDAPAuth, log *zap.Logger,
	now func() time.Time) http.HandlerFunc {
	return budgetedLoginHandler(users, tokens, ldap, log, newLoginBudgets(nil, now))
}

// budgetedLoginHandler is the sign-in handler, counting attempts in budgets.
func budgetedLoginHandler(users user.Store, tokens auth.Store, ldap *LDAPAuth, log *zap.Logger,
	budgets *loginBudgets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil || tokens == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		var req loginRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		addr := clientAddr(r)
		// Two brakes, because one attempt is bounded two ways: how many times this address may guess
		// wrong at all, and how many times anyone may guess at this account. The address budget is
		// checked before any hashing happens, which is what makes it a brake on the work rather than
		// only on the outcome.
		//
		// This only refuses the right people because clientAddr resolves the real client behind a
		// trusted proxy. Keyed on the raw peer address instead, every client behind one proxy shares
		// a budget, and a stranger's failed guesses lock the whole install out of the approval queue.
		// An operator running behind a proxy must set --trusted-proxy or that is what they get.
		spent, err := budgets.addressSpent(r.Context(), addr)
		if err != nil {
			log.Error("server: read the sign-in budget: " + err.Error())
			respondError(w, log, http.StatusServiceUnavailable,
				"sign-in is unavailable: this address's attempt budget could not be read")
			return
		}
		if spent {
			log.Warn("server: sign-in flood from one address", zap.String("address", addr))
			respondError(w, log, http.StatusTooManyRequests,
				"too many failed sign-in attempts from this address, wait a minute")
			return
		}
		allowed, err := budgets.allowAttempt(r.Context(), addr, req.Username)
		if err != nil {
			log.Error("server: spend the sign-in budget: " + err.Error())
			respondError(w, log, http.StatusServiceUnavailable,
				"sign-in is unavailable: this address's attempt budget could not be read")
			return
		}
		if !allowed {
			// A rate-limited attempt is logged too, since a burst against one account is exactly the
			// signal an auditor of authentication activity is looking for.
			log.Warn("server: sign-in rate limited", zap.String("username", req.Username))
			respondError(w, log, http.StatusTooManyRequests, "too many sign-in attempts, wait a minute")
			return
		}
		u, err := user.Authenticate(r.Context(), users, req.Username, req.Password)
		if err != nil && ldap != nil {
			u, err = ldap.Authenticate(r.Context(), req.Username, req.Password)
		}
		req.Password = ""
		if err != nil {
			// Sign-in attempts are deliberately not written to the tamper-evident chain (see the
			// audit gate for why: an unbounded, stranger-driven append that a fail-closed audit store
			// would then turn into a lockout). They live in the server log instead. Only the username
			// and outcome are recorded, never the password or a token; the username is the same actor
			// identity the chain already carries for an authenticated action.
			log.Warn("server: sign-in failed", zap.String("username", req.Username))
			// Only a failure pays into the address budget, so a person who signs in correctly never
			// spends it and an office behind one address is never locked out by its own traffic.
			if err := budgets.recordFailure(r.Context(), addr); err != nil {
				log.Error("server: record a failed sign-in: " + err.Error())
			}
			respondError(w, log, http.StatusUnauthorized, "bad credentials")
			return
		}

		// The token is named for the person and marked as a session, so the chain attributes what
		// they do to them rather than to a row labeled "session casey", and so signing out has
		// something to revoke.
		plain, tok, err := auth.New(u.Username)
		if err != nil {
			log.Error("server: mint session token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not sign in")
			return
		}
		tok.UserID = u.ID
		tok.Kind = auth.KindSession
		expires := time.Now().Add(sessionTokenTTL)
		tok.ExpiresAt = &expires
		if err := tokens.Save(r.Context(), tok); err != nil {
			log.Error("server: save session token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not sign in")
			return
		}
		// A successful sign-in is recorded in the server log, the home for authentication events that
		// the chain excludes, so the trail of who signed in and when exists somewhere durable.
		log.Info("server: sign-in", zap.String("username", u.Username), zap.String("role", string(u.Role)))
		respondJSON(w, log, http.StatusOK,
			loginResponse{Token: plain, Username: u.Username, Role: u.Role}, wantsPretty(r))
	}
}

// createUserHandler creates an account.
func createUserHandler(users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		var req createUserRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Username == "" || req.Password == "" {
			respondError(w, log, http.StatusBadRequest, "username and password are required")
			return
		}
		if _, err := users.FindByUsername(r.Context(), req.Username); err == nil {
			respondError(w, log, http.StatusConflict, "username already exists")
			return
		}
		u, err := user.New(req.Username, req.Password, req.Role)
		req.Password = ""
		if errors.Is(err, user.ErrBadRole) {
			respondError(w, log, http.StatusBadRequest, "role must be admin, operator, or viewer")
			return
		}
		if errors.Is(err, user.ErrUsernameTooLong) {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			log.Error("server: create user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create user")
			return
		}
		// The error names the offending field but never its value, since the profile is personal data.
		if err := req.applyTo(u); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		u.CreatedAt = time.Now()
		// Created by an administrator through the API, not provisioned by a directory, so a
		// directory identity asserting this username cannot later be handed this account.
		u.Source = "local"
		if err := users.Save(r.Context(), u); err != nil {
			log.Error("server: save user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create user")
			return
		}
		respondJSON(w, log, http.StatusCreated, u, wantsPretty(r))
	}
}

// updateUserRequest is the JSON body accepted by PUT /users/{id}. A blank password keeps the
// current one, so an operator can change a role or rename an account without resetting the login.
type updateUserRequest struct {
	// Username is the sign in name. Required.
	Username string `json:"username"`
	// Password sets a new password when non-empty; blank keeps the current one. Never logged.
	Password string `json:"password,omitempty"`
	// Role is admin, operator, or viewer. Required.
	Role user.Role `json:"role"`
	// userProfileRequest carries the profile fields. They are replaced wholesale, so an update sends
	// the profile it wants to end up with rather than only the parts that changed.
	userProfileRequest
}

// updateUserHandler changes an account's username, role, and optionally its password, keeping the
// id and creation time. It rejects a username already taken by a different account.
func updateUserHandler(users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		var req updateUserRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		password := req.Password
		req.Password = ""
		if req.Username == "" {
			respondError(w, log, http.StatusBadRequest, "username is required")
			return
		}
		if err := user.CheckUsername(req.Username); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if !user.ValidRole(req.Role) {
			respondError(w, log, http.StatusBadRequest, "role must be admin, operator, or viewer")
			return
		}
		id := r.PathValue("id")
		u, err := users.Get(r.Context(), id)
		if errors.Is(err, user.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			log.Error("server: get user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read user")
			return
		}
		if clash, err := users.FindByUsername(r.Context(), req.Username); err == nil && clash.ID != id {
			respondError(w, log, http.StatusConflict, "username already exists")
			return
		}

		u.Username = req.Username
		u.Role = req.Role
		if err := req.applyTo(u); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if password != "" {
			if err := u.SetPassword(password); err != nil {
				log.Error("server: hash password: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not update user")
				return
			}
		}
		// Demoting is the other way to reach zero administrators, so the write carries the same
		// guard the delete does and in the same statement.
		applied, err := users.UpdateUnlessLastAdmin(r.Context(), u)
		if err != nil {
			log.Error("server: update user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update user")
			return
		}
		if !applied {
			respondError(w, log, http.StatusConflict, "cannot demote the last admin")
			return
		}
		respondJSON(w, log, http.StatusOK, u, wantsPretty(r))
	}
}

// listUsersHandler returns all accounts without password material.
func listUsersHandler(users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		list, err := users.List(r.Context())
		if err != nil {
			log.Error("server: list users: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list users")
			return
		}
		capped, total := cappedList(list)
		respondJSON(w, log, http.StatusOK,
			listUsersResponse{Users: capped, Count: len(capped), Total: total}, wantsPretty(r))
	}
}

// deleteUserHandler removes an account. Its tokens stop working on their next use, and its forge
// account links end before it goes, each recorded on the chain as unlinked, so a forge account it
// linked can be linked again, no pull request comment acts as an account that is gone, and the
// chain and the links agree.
func deleteUserHandler(users user.Store, links forgelink.Store, audits audit.Store,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		id := r.PathValue("id")
		if !unlinkDeletedUser(w, r, users, links, audits, log, id) {
			return
		}
		// The count and the delete are one statement in the store. Asking first and deleting after
		// let two concurrent deletes of the last two admins both see a survivor and both proceed.
		deleted, err := users.DeleteUnlessLastAdmin(r.Context(), id)
		if err == nil && !deleted {
			respondError(w, log, http.StatusConflict, "cannot delete the last admin")
			return
		}
		if errors.Is(err, user.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			log.Error("server: delete user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete user")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// unlinkDeletedUser ends the forge links of the account id that is about to be deleted, recording
// each on the chain as unlinked before it is removed, and reports whether the delete may go on. It
// writes the refusal when it may not.
//
// A link left behind would act as nobody, since its account is gone, but it would hold the forge
// account, which then could never be linked to anybody again, and the chain would show it linked
// for good. The links end first and fail closed: an unlink the chain cannot record keeps that link,
// and the account, and refuses the delete. An account that is missing, or the install's last admin,
// is answered before any link is touched, so a delete that is refused anyway ends no link.
func unlinkDeletedUser(w http.ResponseWriter, r *http.Request, users user.Store,
	links forgelink.Store, audits audit.Store, log *zap.Logger, id string) bool {
	if links == nil {
		return true
	}
	ctx := r.Context()
	owned, err := links.ForUser(ctx, id)
	if err != nil {
		log.Error("server: list a deleted user's forge links: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not delete user")
		return false
	}
	if len(owned) == 0 {
		return true
	}
	target, err := users.Get(ctx, id)
	if errors.Is(err, user.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "user not found")
		return false
	}
	if err != nil {
		log.Error("server: read user: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not delete user")
		return false
	}
	if target.Role == user.RoleAdmin && lastAdmin(ctx, users) {
		respondError(w, log, http.StatusConflict, "cannot delete the last admin")
		return false
	}
	who, _ := recordedFrom(ctx)
	name, typ := who.Name, who.Type
	if actor, ok := actorFrom(ctx); ok && name == "" {
		name, typ = actor.Name, actor.Type
	}
	if _, err := forgelink.UnlinkUser(ctx, links, id, func(l *forgelink.Link) error {
		return recordForgeLink(ctx, audits, forgelink.ActionUnlinked, l, name, typ)
	}); err != nil {
		log.Error("server: unlink a deleted user's forge accounts: " + err.Error())
		respondError(w, log, http.StatusServiceUnavailable, "refused: the account's forge links could "+
			"not be recorded as unlinked in the audit trail, so the account was kept")
		return false
	}
	return true
}

// lastAdmin reports whether the install holds one admin or none, which the account about to be
// deleted would leave with nobody to manage it. A store that cannot answer counts as last, so an
// unlink never runs ahead of a delete that cannot be checked.
func lastAdmin(ctx context.Context, users user.Store) bool {
	list, err := users.List(ctx)
	if err != nil {
		return true
	}
	admins := 0
	for _, u := range list {
		if u.Role == user.RoleAdmin {
			admins++
		}
	}
	return admins <= 1
}
