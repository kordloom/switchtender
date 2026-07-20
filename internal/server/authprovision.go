package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/auth"
	"github.com/dcadolph/switchtender/internal/user"
)

// mintSessionToken creates and stores a session token owned by u and returns its plaintext once.
// Every single sign-on flow mints through here, so an SSO session looks exactly like one from the
// password login.
func mintSessionToken(ctx context.Context, tokens auth.Store, u *user.User) (string, error) {
	plain, tok, err := auth.New("sso " + u.Username)
	if err != nil {
		return "", err
	}
	tok.UserID = u.ID
	expires := time.Now().Add(sessionTokenTTL)
	tok.ExpiresAt = &expires
	if err := tokens.Save(ctx, tok); err != nil {
		return "", err
	}
	return plain, nil
}

// roleForGroups returns the role for the first group found in roleMap, or defaultRole, so a directory
// or token group drives an account's role. Group names are matched case-insensitively.
func roleForGroups(groups []string, roleMap map[string]user.Role, defaultRole user.Role) user.Role {
	for _, g := range groups {
		if role, ok := roleMap[strings.ToLower(g)]; ok {
			return role
		}
	}
	return defaultRole
}

// claimGroups reads a groups claim, which a token may encode as a list of strings, a list of any, or a
// single string, into a slice, so group-to-role mapping works across issuers.
func claimGroups(v any) []string {
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

// provisionFromDirectory finds the local account for username or creates it with role. When driveRole
// is true, an existing account's role is updated to match, so a directory or token is the source of
// truth for authorization. source labels the log line, for example ldap or jwt.
func provisionFromDirectory(ctx context.Context, users user.Store, log *zap.Logger,
	username string, role user.Role, driveRole bool, source string) (*user.User, error) {
	u, err := users.FindByUsername(ctx, username)
	if err == nil {
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
	pw, err := randToken()
	if err != nil {
		return nil, err
	}
	u, err = user.New(username, pw, role)
	if err != nil {
		return nil, err
	}
	if err := users.Save(ctx, u); err != nil {
		return nil, err
	}
	log.Info(source + ": provisioned account " + username + " as " + string(role))
	return u, nil
}
