package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kordloom/switchtender/internal/safedial"
)

// Directory sign-in (OIDC, SAML, LDAP, JWT) is implemented in the paid module and reached through
// the seam package's Directory interface. What the module needs from this package is exported here
// and nowhere else: the guarded identity-provider client, the helpers that provision an account and
// mint its session, and the sign-in audit record, so every sign-in path keeps going through the same
// doors the password login uses.

// idpFetchTimeout bounds one request to an identity provider.
const idpFetchTimeout = 30 * time.Second

// DefaultJWTAgentClaim is the claim a JWT issuer sets to agent to mark a token as an AI agent's.
const DefaultJWTAgentClaim = "st_actor_type"

// ErrLDAPAuth is the refusal an LDAP sign-in returns when the directory rejects the credentials.
var ErrLDAPAuth = errors.New("ldap authentication failed")

// ErrJWTAuth is the refusal a JWT sign-in returns when the token does not verify.
var ErrJWTAuth = errors.New("jwt authentication failed")

// IDPClient returns the client every identity provider request goes through: the SAML metadata
// document, OIDC discovery with its key set and code exchange, and a JWT issuer's key set. Each
// address is operator configuration this server follows on its own network, so it dials through
// the same guard every other configured address does. A redirect is still followed, since an
// identity provider may answer one, and each hop meets the same dial check.
func IDPClient() *http.Client {
	return &http.Client{Transport: safedial.Transport(), Timeout: idpFetchTimeout}
}

// RandToken returns a URL-safe 256-bit random string for state, nonce, and provisioned passwords.
// It returns an error rather than an empty string when the system RNG fails, so a caller never
// proceeds with an empty security value.
func RandToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
