package server

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
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
	now := time.Now()
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
	if !ok || time.Since(w.start) > loginWindowLength {
		return false
	}
	return w.count >= max
}

// record consumes one attempt for the key, opening a window when none is current.
func (l *loginLimiter) record(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
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

// clientAddr returns the request's client host without the port, the stable half of the limiter
// key. The remote address is used as seen; forwarding headers are spoofable and are not trusted.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginHandler authenticates a username and password and mints a session token owned by the user.
// Attempts are rate limited per client and username so stolen password lists cannot be replayed
// at full speed.
func loginHandler(users user.Store, tokens auth.Store, ldap *LDAPAuth, log *zap.Logger) http.HandlerFunc {
	limiter := &loginLimiter{windows: make(map[string]*loginWindow)}
	// The address budget is kept in its own limiter so its keys cannot collide with the per-username
	// ones and its larger cap applies to nothing else.
	addresses := &loginLimiter{windows: make(map[string]*loginWindow)}
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
		if addresses.spent(addr, loginAddressMax) {
			log.Warn("server: sign-in flood from one address", zap.String("address", addr))
			respondError(w, log, http.StatusTooManyRequests,
				"too many failed sign-in attempts from this address, wait a minute")
			return
		}
		if !limiter.allow(addr + "\x00" + req.Username) {
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
			addresses.record(addr)
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
		respondJSON(w, log, http.StatusOK,
			listUsersResponse{Users: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteUserHandler removes an account. Its tokens stop working on their next use.
func deleteUserHandler(users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		id := r.PathValue("id")
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
