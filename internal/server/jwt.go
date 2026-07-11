package server

import (
	"context"
	"errors"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/user"
)

// ErrJWTAuth is returned when a bearer JWT is refused, so the auth gate reports unauthorized without
// revealing why the token was rejected.
var ErrJWTAuth = errors.New("jwt authentication failed")

// JWTAuth authenticates a request that carries a signed JWT as its bearer token, validating it against
// an issuer's published keys and mapping its claims to a local account. It lets a service present a
// JWT minted elsewhere, such as by jwtmint, instead of a Yardmaster token.
type JWTAuth struct {
	// verifier checks a token's signature against the issuer's keys and its issuer, audience, and
	// expiry.
	verifier *oidc.IDTokenVerifier
	// users provisions and looks up local accounts.
	users user.Store
	// usernameClaim is the claim naming the account, for example sub or email.
	usernameClaim string
	// groupsClaim is the claim holding the user's groups, used with roleMap to set a role.
	groupsClaim string
	// roleMap maps a group to a role, so a token's groups drive the account's role.
	roleMap map[string]user.Role
	// defaultRole is granted when no group maps to a role.
	defaultRole user.Role
	// log records sign-in activity without token material.
	log *zap.Logger
}

// NewJWTAuth builds a JWTAuth. It fetches the issuer's keys from jwksURL and verifies the issuer, and
// the audience when one is given.
func NewJWTAuth(ctx context.Context, jwksURL, issuer, audience, usernameClaim, groupsClaim string,
	defaultRole user.Role, roleMap map[string]user.Role, users user.Store, log *zap.Logger) (*JWTAuth, error) {
	if jwksURL == "" || issuer == "" {
		return nil, errors.New("jwt needs a jwks url and an issuer")
	}
	if users == nil {
		return nil, errors.New("jwt needs a user store")
	}
	if !user.ValidRole(defaultRole) {
		defaultRole = user.RoleViewer
	}
	if usernameClaim == "" {
		usernameClaim = "sub"
	}
	verifier := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, jwksURL),
		&oidc.Config{ClientID: audience, SkipClientIDCheck: audience == ""})
	return &JWTAuth{
		verifier: verifier, users: users, usernameClaim: usernameClaim, groupsClaim: groupsClaim,
		roleMap: roleMap, defaultRole: defaultRole, log: log,
	}, nil
}

// Authenticate verifies raw and provisions the account it names. A verification failure is an
// authentication failure, not a server error, so a bad or expired token is simply refused.
func (j *JWTAuth) Authenticate(ctx context.Context, raw string) (*user.User, error) {
	tok, err := j.verifier.Verify(ctx, raw)
	if err != nil {
		j.log.Warn("jwt: verify: " + err.Error())
		return nil, ErrJWTAuth
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, ErrJWTAuth
	}
	username, _ := claims[j.usernameClaim].(string)
	if username == "" {
		return nil, ErrJWTAuth
	}
	role := roleForGroups(claimGroups(claims[j.groupsClaim]), j.roleMap, j.defaultRole)
	return provisionFromDirectory(ctx, j.users, j.log, username, role, len(j.roleMap) > 0, "jwt")
}
