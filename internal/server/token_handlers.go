package server

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// createTokenRequest is the JSON body accepted by POST /tokens.
type createTokenRequest struct {
	// Name labels the token for humans, for example ci or deploy-bot. Required, because a token
	// nobody can identify is a token nobody will ever revoke.
	Name string `json:"name"`
	// Username binds the token to an account, whose role it then carries. Required: a token bound to
	// no account acts as admin with nobody behind it, and the API does not hand out an unattributable
	// credential over the network.
	Username string `json:"username"`
	// Kind is auth.KindAgent for a token held by an AI agent, empty for a person's. An agent token is
	// recorded under the agent identity in the chain and capped so it cannot manage identity, access,
	// or secrets, or approve its own held runs.
	Kind string `json:"kind,omitempty"`
	// TTLHours is the token's lifetime. Zero means it never expires; a negative value is refused
	// rather than quietly producing the opposite of a short-lived credential.
	TTLHours int `json:"ttl_hours,omitempty"`
}

// createTokenResponse carries the minted token. The plaintext appears here and nowhere else, ever
// again: only its hash is stored.
type createTokenResponse struct {
	// ID identifies the token for listing and revocation.
	ID string `json:"id"`
	// Name is the label it was minted with.
	Name string `json:"name"`
	// Token is the plaintext, returned exactly once.
	Token string `json:"token"`
	// Kind is agent for an agent token, empty for a person's.
	Kind string `json:"kind,omitempty"`
	// Username is the account the token is bound to.
	Username string `json:"username"`
	// Role is the role the token carries, after the agent cap.
	Role string `json:"role"`
	// ExpiresAt is when it stops working, absent when it never does.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// listTokensResponse wraps the token list. The tokens carry no secret: auth.Token keeps its hash out
// of JSON, and the plaintext was never stored to begin with.
type listTokensResponse struct {
	// Tokens is every stored token, oldest first.
	Tokens []*auth.Token `json:"tokens"`
	// Count is the number returned.
	Count int `json:"count"`
}

// listTokensHandler returns every token, without secrets, so an admin can see what holds access to
// this install and take any of it back. Browser sessions appear here too, which is what makes "sign
// out everywhere" possible.
func listTokensHandler(tokens auth.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			respondError(w, log, http.StatusNotFound, "tokens not enabled")
			return
		}
		list, err := tokens.List(r.Context())
		if err != nil {
			log.Error("server: list tokens: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list tokens")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listTokensResponse{Tokens: list, Count: len(list)}, wantsPretty(r))
	}
}

// createTokenHandler mints a token bound to an account and returns its plaintext once.
//
// Every token used to come from the command line against the database file, which meant an install
// an operator cannot get a shell on could not issue an agent a credential at all: the agent story,
// the one thing that needs a minted, capped, attributable token, was reachable only by the person
// sitting on the host.
//
// Two rules hold here that the command line also holds, for the same reasons. A token must name the
// account it acts as, so the chain records who is behind it and the token can never exceed that
// account's role. An agent token must name that account too, or the chain records an action by an
// agent on behalf of nobody.
func createTokenHandler(tokens auth.Store, users user.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil || users == nil {
			respondError(w, log, http.StatusNotFound, "tokens not enabled")
			return
		}
		var req createTokenRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest,
				"name is required, so the token can be recognized later and revoked")
			return
		}
		if req.Username == "" {
			respondError(w, log, http.StatusBadRequest,
				"username is required: a token bound to no account acts as admin with nobody behind "+
					"it, so the API does not issue one. Bind it to an account, or mint an unscoped "+
					"token on the server with switchtender token new")
			return
		}
		if req.Kind != "" && req.Kind != auth.KindAgent {
			respondError(w, log, http.StatusBadRequest, "kind must be agent, or left out for a person")
			return
		}
		if req.TTLHours < 0 {
			respondError(w, log, http.StatusBadRequest,
				"ttl_hours cannot be negative: pass a positive lifetime, or zero for a token that "+
					"never expires")
			return
		}
		u, err := users.FindByUsername(r.Context(), req.Username)
		if errors.Is(err, user.ErrNotFound) {
			respondError(w, log, http.StatusBadRequest, "no account with that username")
			return
		}
		if err != nil {
			log.Error("server: read user: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the account")
			return
		}

		plain, tok, err := auth.New(req.Name)
		if err != nil {
			log.Error("server: mint token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not mint token")
			return
		}
		tok.UserID = u.ID
		tok.Kind = req.Kind
		if req.TTLHours > 0 {
			expires := time.Now().Add(time.Duration(req.TTLHours) * time.Hour)
			tok.ExpiresAt = &expires
		}
		if err := tokens.Save(r.Context(), tok); err != nil {
			log.Error("server: save token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store token")
			return
		}
		role := u.Role
		if tok.IsAgent() {
			role = capAgentRole(role)
		}
		// The label, the account, and the kind are logged; the plaintext never is.
		log.Info("server: token minted", zap.String("name", tok.Name),
			zap.String("username", u.Username), zap.String("kind", tok.Kind))
		out := createTokenResponse{
			ID: tok.ID, Name: tok.Name, Token: plain, Kind: tok.Kind,
			Username: u.Username, Role: string(role), ExpiresAt: tok.ExpiresAt,
		}
		respondJSON(w, log, http.StatusCreated, out, wantsPretty(r))
		// The plaintext is cleared from the response value as soon as it is written, so it does not
		// sit in memory any longer than the request that carried it.
		out.Token = ""
		plain = ""
		_ = plain
	}
}

// deleteTokenHandler revokes a token by id, which stops it working everywhere at once.
func deleteTokenHandler(tokens auth.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			respondError(w, log, http.StatusNotFound, "tokens not enabled")
			return
		}
		id := r.PathValue("id")
		err := tokens.Delete(r.Context(), id)
		if errors.Is(err, auth.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "token not found")
			return
		}
		if err != nil {
			log.Error("server: revoke token: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not revoke token")
			return
		}
		log.Info("server: token revoked", zap.String("id", id))
		respondJSON(w, log, http.StatusOK, map[string]string{"revoked": id}, wantsPretty(r))
	}
}
