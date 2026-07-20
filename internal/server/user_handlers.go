package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/auth"
	"github.com/dcadolph/switchtender/internal/user"
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

// createUserRequest is the JSON body accepted by POST /users.
type createUserRequest struct {
	// Username is the unique sign in name. Required.
	Username string `json:"username"`
	// Password is the initial password. Required, never logged.
	Password string `json:"password"`
	// Role is admin, operator, or viewer. Required.
	Role user.Role `json:"role"`
}

// listUsersResponse wraps the user list, password hashes excluded by the model's json tags.
type listUsersResponse struct {
	// Users is the ordered list.
	Users []*user.User `json:"users"`
	// Count is the number returned.
	Count int `json:"count"`
}

// loginHandler authenticates a username and password and mints a session token owned by the user.
func loginHandler(users user.Store, tokens auth.Store, ldap *LDAPAuth, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil || tokens == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		u, err := user.Authenticate(r.Context(), users, req.Username, req.Password)
		if err != nil && ldap != nil {
			u, err = ldap.Authenticate(r.Context(), req.Username, req.Password)
		}
		req.Password = ""
		if err != nil {
			respondError(w, log, http.StatusUnauthorized, "bad credentials")
			return
		}

		plain, tok, err := auth.New("session " + u.Username)
		if err != nil {
			log.Error("server: mint session token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not sign in")
			return
		}
		tok.UserID = u.ID
		expires := time.Now().Add(sessionTokenTTL)
		tok.ExpiresAt = &expires
		if err := tokens.Save(r.Context(), tok); err != nil {
			log.Error("server: save session token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not sign in")
			return
		}
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
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

		if u.Role == user.RoleAdmin && req.Role != user.RoleAdmin {
			last, err := isLastAdmin(r.Context(), users, id)
			if err != nil {
				log.Error("server: count admins: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not update user")
				return
			}
			if last {
				respondError(w, log, http.StatusConflict, "cannot demote the last admin")
				return
			}
		}

		u.Username = req.Username
		u.Role = req.Role
		if password != "" {
			if err := u.SetPassword(password); err != nil {
				log.Error("server: hash password: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not update user")
				return
			}
		}
		if err := users.Update(r.Context(), u); err != nil {
			log.Error("server: update user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update user")
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

// isLastAdmin reports whether the account with id is the only admin, so demoting or deleting it would
// leave the install with no administrator and lock everyone out of the admin-gated endpoints.
func isLastAdmin(ctx context.Context, users user.Store, id string) (bool, error) {
	list, err := users.List(ctx)
	if err != nil {
		return false, err
	}
	target, others := false, 0
	for _, u := range list {
		if u.Role != user.RoleAdmin {
			continue
		}
		if u.ID == id {
			target = true
		} else {
			others++
		}
	}
	return target && others == 0, nil
}

// deleteUserHandler removes an account. Its tokens stop working on their next use.
func deleteUserHandler(users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if users == nil {
			respondError(w, log, http.StatusNotFound, "accounts not enabled")
			return
		}
		id := r.PathValue("id")
		last, err := isLastAdmin(r.Context(), users, id)
		if err != nil {
			log.Error("server: count admins: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete user")
			return
		}
		if last {
			respondError(w, log, http.StatusConflict, "cannot delete the last admin")
			return
		}
		err = users.Delete(r.Context(), id)
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
