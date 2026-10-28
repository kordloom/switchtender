package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// MintSessionToken creates and stores a session token owned by u and returns its plaintext once.
// Every single sign-on flow mints through here, so an SSO session looks exactly like one from the
// password login.
func MintSessionToken(ctx context.Context, tokens auth.Store, u *user.User) (string, error) {
	plain, tok, err := auth.New(u.Username)
	if err != nil {
		return "", err
	}
	tok.UserID = u.ID
	tok.Kind = auth.KindSession
	expires := time.Now().Add(sessionTokenTTL)
	tok.ExpiresAt = &expires
	if err := tokens.Save(ctx, tok); err != nil {
		return "", err
	}
	return plain, nil
}

// RoleForGroups returns the role for the first group found in roleMap, or defaultRole, so a directory
// or token group drives an account's role. Group names are matched case-insensitively.
func RoleForGroups(groups []string, roleMap map[string]user.Role, defaultRole user.Role) user.Role {
	for _, g := range groups {
		if role, ok := roleMap[strings.ToLower(g)]; ok {
			return role
		}
	}
	return defaultRole
}

// ClaimGroups reads a groups claim, which a token may encode as a list of strings, a list of any, or a
// single string, into a slice, so group-to-role mapping works across issuers.
func ClaimGroups(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ErrForeignAccount is returned when a directory identity names an account another source owns.
var ErrForeignAccount = errors.New("account belongs to another source")

// AccountSource names an account's origin for a refusal message, spelling the empty case out.
func AccountSource(s string) string {
	if s == "" {
		return "a local administrator"
	}
	return s
}

// ProvisionFromDirectory finds the local account for username or creates it with role. When driveRole
// is true, an existing account's role is updated to match, so a directory or token is the source of
// truth for authorization. source labels the log line, for example ldap or jwt.
func ProvisionFromDirectory(ctx context.Context, users user.Store, log *zap.Logger,
	username string, role user.Role, driveRole bool, source string) (*user.User, error) {
	u, err := users.FindByUsername(ctx, username)
	if err == nil {
		// An account another source owns is not this one's to sign in as. Matching on username alone
		// meant an identity provider asserting the name of a local administrator was handed that
		// administrator's account and role, and with driveRole on it could raise its own role too.
		// Safe on the defaults, where the username is a subject or a directory search result, and
		// account takeover as soon as an operator points the username claim at an email attribute
		// against an issuer that lets a user assert their own address. OIDC already refuses the
		// equivalent when the provider does not vouch for the address.
		//
		// An account with no source recorded predates the field, so it cannot be told apart from one
		// this same directory provisioned. Those are still adopted, because refusing them would lock
		// out every directory user on upgrade, and the ambiguity is logged so it is visible rather
		// than assumed away.
		switch u.Source {
		case source:
		case "":
			log.Warn(source + ": signing in to account " + username + ", which records no source. " +
				"It cannot be told apart from a local account of the same name; set its source to " +
				"remove the ambiguity")
		default:
			return nil, fmt.Errorf("%w: account %q belongs to %q, not %q", ErrForeignAccount,
				username, AccountSource(u.Source), source)
		}
		if driveRole && u.Role != role {
			u.Role = role
			if err := users.Update(ctx, u); err != nil {
				return nil, err
			}
			log.Info(source + ": set " + username + " to " + string(role) + " from its groups")
		}
		return u, nil
	}
	if !errors.Is(err, user.ErrNotFound) {
		return nil, err
	}
	pw, err := RandToken()
	if err != nil {
		return nil, err
	}
	u, err = user.New(username, pw, role)
	if err != nil {
		return nil, err
	}
	u.Source = source
	if err := users.Save(ctx, u); err != nil {
		return nil, err
	}
	log.Info(source + ": provisioned account " + username + " as " + string(role))
	return u, nil
}
