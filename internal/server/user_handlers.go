package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/user"
)

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
func loginHandler(users user.Store, tokens auth.Store, log *zap.Logger) http.HandlerFunc {
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
		err := users.Delete(r.Context(), r.PathValue("id"))
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
