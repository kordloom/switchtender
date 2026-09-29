package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestARequestRefusedForItsRoleIsOnTheRecord pins what the audit page promises: every request that
// would change the install is recorded before it runs, so a refused one is on the record too. A
// request refused later by its handler always was. One refused at the door for its caller's role was
// not, so an operator writing a policy or approving a run, and an agent reaching for accounts, left no
// entry and no receipt. A read refused for its role is still not recorded, since reads never are.
func TestARequestRefusedForItsRoleIsOnTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	audits := audit.NewMemStore()
	account := func(name string, role user.Role) *user.User {
		t.Helper()
		u, err := user.New(name, "a-password-long-enough", role)
		if err != nil {
			t.Fatalf("user.New(%s) error = %v", name, err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		return u
	}
	token := func(label string, owner *user.User, kind string) string {
		t.Helper()
		plain, tok, err := auth.New(label)
		if err != nil {
			t.Fatalf("auth.New(%s) error = %v", label, err)
		}
		tok.UserID, tok.Kind = owner.ID, kind
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("save token %s: %v", label, err)
		}
		return plain
	}
	jane := token("jane-cli", account("jane", user.RoleOperator), "")
	bot := token("deploy-bot", account("owner", user.RoleAdmin), auth.KindAgent)
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()

	tests := []struct {
		Token      string
		Method     string
		Path       string
		Body       string
		WantActor  string
		WantType   string
		WantRecord bool
	}{{ // Test 0: An operator writing a policy.
		Token: jane, Method: http.MethodPost, Path: "/v1/policies", Body: `{"name":"x"}`,
		WantActor: "jane-cli", WantType: "token", WantRecord: true,
	}, { // Test 1: An operator approving a held run.
		Token: jane, Method: http.MethodPost, Path: "/v1/runs/run_1/approve", Body: `{}`,
		WantActor: "jane-cli", WantType: "token", WantRecord: true,
	}, { // Test 2: An agent reaching for the accounts its human could create.
		Token: bot, Method: http.MethodPost, Path: "/v1/users", Body: `{"username":"x"}`,
		WantActor: "deploy-bot", WantType: "agent", WantRecord: true,
	}, { // Test 3: A read refused for its role, which is not a change.
		Token: jane, Method: http.MethodGet, Path: "/v1/audit",
	}}
	for _, test := range tests {
		before, err := audits.Chain(ctx)
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		req := httptest.NewRequest(test.Method, test.Path, strings.NewReader(test.Body))
		req.Header.Set("Authorization", "Bearer "+test.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403", test.Method, test.Path, rec.Code)
		}
		after, err := audits.Chain(ctx)
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		if !test.WantRecord {
			if len(after) != len(before) {
				t.Errorf("%s %s, a refused read, appended %d entries", test.Method, test.Path,
					len(after)-len(before))
			}
			continue
		}
		if len(after) != len(before)+1 {
			t.Fatalf("%s %s refused for its role appended %d entries, want 1", test.Method, test.Path,
				len(after)-len(before))
		}
		e := after[len(after)-1]
		if e.Path != test.Path || e.Actor != test.WantActor || e.ActorType != test.WantType {
			t.Errorf("%s %s recorded %s by %s (%s), want it by %s (%s)", test.Method, test.Path, e.Path,
				e.Actor, e.ActorType, test.WantActor, test.WantType)
		}
		if rec.Header().Get(AuditReceiptHeader) != audit.Receipt(e) {
			t.Errorf("%s %s answered receipt %q, want %q", test.Method, test.Path,
				rec.Header().Get(AuditReceiptHeader), audit.Receipt(e))
		}
	}
}
