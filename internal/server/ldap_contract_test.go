package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jimlambrt/gldap"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// startTestDirectory runs an embedded LDAP directory holding one person, ada, in the admins
// group, with a service account the server binds first. It speaks the same wire protocol a real
// directory does, which is the point: the LDAP path was the one paid sign-in surface with zero
// coverage anywhere, mocked or real.
func startTestDirectory(t *testing.T) string {
	t.Helper()
	const (
		serviceDN   = "cn=svc,dc=example,dc=com"
		servicePass = "svc-secret"
		adaDN       = "uid=ada,ou=people,dc=example,dc=com"
		adaPass     = "correct-horse"
	)
	mux, err := gldap.NewMux()
	if err != nil {
		t.Fatalf("gldap.NewMux: %v", err)
	}
	err = mux.Bind(func(w *gldap.ResponseWriter, r *gldap.Request) {
		resp := r.NewBindResponse(gldap.WithResponseCode(gldap.ResultInvalidCredentials))
		defer func() { _ = w.Write(resp) }()
		m, err := r.GetSimpleBindMessage()
		if err != nil {
			return
		}
		pass := string(m.Password)
		if (m.UserName == serviceDN || m.UserName == "svc") && pass == servicePass {
			resp.SetResultCode(gldap.ResultSuccess)
			return
		}
		if (m.UserName == adaDN || m.UserName == "ada") && pass == adaPass {
			resp.SetResultCode(gldap.ResultSuccess)
		}
	})
	if err != nil {
		t.Fatalf("mux.Bind: %v", err)
	}
	err = mux.Search(func(w *gldap.ResponseWriter, r *gldap.Request) {
		m, err := r.GetSearchMessage()
		if err != nil {
			return
		}
		done := r.NewSearchDoneResponse()
		defer func() { _ = w.Write(done) }()
		// The only person in the directory. The filter arrives already escaped by the client.
		if m.Filter == "(uid=ada)" {
			entry := r.NewSearchResponseEntry(adaDN, gldap.WithAttributes(map[string][]string{
				"memberOf": {"cn=admins,ou=groups,dc=example,dc=com"},
			}))
			if err := w.Write(entry); err != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("mux.Search: %v", err)
	}
	s, err := gldap.NewServer()
	if err != nil {
		t.Fatalf("gldap.NewServer: %v", err)
	}
	if err := s.Router(mux); err != nil {
		t.Fatalf("Router: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	go func() { _ = s.Run(addr) }()
	t.Cleanup(func() { _ = s.Stop() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("embedded directory never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLDAPAuthenticateAgainstARealDirectory drives the whole arc over the wire: service bind,
// user search, user bind, group-to-role mapping, and just-in-time provisioning, plus the
// refusals that matter. Every branch here is a sign-in decision on a paid tier.
func TestLDAPAuthenticateAgainstARealDirectory(t *testing.T) {
	t.Parallel()
	addr := startTestDirectory(t)
	users := user.NewMemStore()
	l, err := NewLDAPAuth("ldap://"+addr, "cn=svc,dc=example,dc=com", "svc-secret",
		"dc=example,dc=com", "(uid=%s)", user.RoleViewer,
		map[string]user.Role{"cn=admins,ou=groups,dc=example,dc=com": user.RoleAdmin},
		users, zap.NewNop())
	if err != nil {
		t.Fatalf("NewLDAPAuth: %v", err)
	}
	ctx := context.Background()

	// The whole arc: found, bound, mapped, provisioned.
	u, err := l.Authenticate(ctx, "ada", "correct-horse")
	if err != nil {
		t.Fatalf("Authenticate(ada) error = %v", err)
	}
	if u.Username != "ada" || u.Role != user.RoleAdmin {
		t.Errorf("provisioned %q with role %q, want ada mapped to admin through her group", u.Username, u.Role)
	}
	if stored, err := users.FindByUsername(ctx, "ada"); err != nil || stored == nil {
		t.Errorf("ada was not provisioned into the user store: %v", err)
	}

	// The refusals, each one a different way in.
	for _, test := range []struct {
		Name, User, Pass string
	}{
		{"wrong password", "ada", "wrong"},
		{"unknown user", "nobody", "whatever"},
		{"empty password never reaches the wire", "ada", ""},
	} {
		if _, err := l.Authenticate(ctx, test.User, test.Pass); !errors.Is(err, ErrLDAPAuth) {
			t.Errorf("%s: Authenticate = %v, want ErrLDAPAuth", test.Name, err)
		}
	}
}
