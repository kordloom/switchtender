package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-ldap/ldap/v3"
	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/user"
)

// ErrLDAPAuth is returned when an LDAP sign-in is refused, so the login handler reports bad
// credentials without revealing whether the username or the password was wrong.
var ErrLDAPAuth = errors.New("ldap authentication failed")

// LDAPAuth verifies a username and password against an LDAP directory and provisions a local account
// for the user on first sign-in, so a directory can drive sign-in alongside local accounts and OIDC.
type LDAPAuth struct {
	// url is the directory URL, for example ldaps://ldap.example.com:636.
	url string
	// bindDN is the service account used to search for a user, empty for an anonymous search.
	bindDN string
	// bindPassword is the service account password.
	bindPassword string
	// baseDN is the search base for finding a user, for example ou=people,dc=example,dc=com.
	baseDN string
	// userFilter is the search filter with one %s for the username, for example (uid=%s).
	userFilter string
	// users provisions and looks up local accounts.
	users user.Store
	// defaultRole is granted when no group maps to a role.
	defaultRole user.Role
	// roleMap maps a lowercased group name, the memberOf value, to a role, so a directory group drives
	// a user's role on every sign-in. Empty leaves everyone on the default role.
	roleMap map[string]user.Role
	// log records sign-in activity without secret material.
	log *zap.Logger
}

// NewLDAPAuth builds an LDAPAuth. It validates the required settings so a misconfiguration fails at
// startup rather than at the first sign-in.
func NewLDAPAuth(url, bindDN, bindPassword, baseDN, userFilter string,
	defaultRole user.Role, roleMap map[string]user.Role, users user.Store, log *zap.Logger) (*LDAPAuth, error) {
	if url == "" || baseDN == "" || userFilter == "" {
		return nil, errors.New("ldap needs a url, a base dn, and a user filter")
	}
	if users == nil {
		return nil, errors.New("ldap needs a user store")
	}
	if !user.ValidRole(defaultRole) {
		defaultRole = user.RoleViewer
	}
	return &LDAPAuth{
		url: url, bindDN: bindDN, bindPassword: bindPassword, baseDN: baseDN,
		userFilter: userFilter, users: users, defaultRole: defaultRole, roleMap: roleMap, log: log,
	}, nil
}

// Authenticate searches the directory for the username, binds as that user with the given password to
// prove it, and provisions a local account. An empty password is refused, since an empty bind password
// is an unauthenticated bind that a directory can accept.
func (l *LDAPAuth) Authenticate(ctx context.Context, username, password string) (*user.User, error) {
	if password == "" {
		return nil, ErrLDAPAuth
	}
	conn, err := ldap.DialURL(l.url)
	if err != nil {
		l.log.Warn("ldap: dial: " + err.Error())
		return nil, ErrLDAPAuth
	}
	defer func() { _ = conn.Close() }()

	if l.bindDN != "" {
		if err := conn.Bind(l.bindDN, l.bindPassword); err != nil {
			l.log.Warn("ldap: service bind: " + err.Error())
			return nil, ErrLDAPAuth
		}
	}

	req := ldap.NewSearchRequest(l.baseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0,
		false, fmt.Sprintf(l.userFilter, ldap.EscapeFilter(username)), []string{"dn", "memberOf"}, nil)
	res, err := conn.Search(req)
	if err != nil {
		l.log.Warn("ldap: search: " + err.Error())
		return nil, ErrLDAPAuth
	}
	if len(res.Entries) != 1 {
		return nil, ErrLDAPAuth
	}
	entry := res.Entries[0]

	if err := conn.Bind(entry.DN, password); err != nil {
		return nil, ErrLDAPAuth
	}
	role := roleForGroups(entry.GetAttributeValues("memberOf"), l.roleMap, l.defaultRole)
	return provisionFromDirectory(ctx, l.users, l.log, username, role, len(l.roleMap) > 0, "ldap")
}
