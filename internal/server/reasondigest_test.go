package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheRequestFingerprintNeverCommitsAReasonsText pins that the text an approver types is never
// committed to the chain as written. The gate commits a keyed fingerprint of every request body,
// and its key is stored beside the entry, so a body carrying the reason would let anybody with the
// database confirm a guess of the original text long after the reason was redacted. The decision
// entry commits the masked text instead, under a random value a redaction deletes, and the
// request's fingerprint commits the body with that text withheld.
func TestTheRequestFingerprintNeverCommitsAReasonsText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path is the route, relative to the held run.
		Path string
		// Body is what is sent.
		Body string
	}{{ // Test 0: A rejection's reason.
		Path: "/reject", Body: `{"reason":"refused, the requester is on leave until the tenth"}`,
	}, { // Test 1: An approval's reason.
		Path: "/approve", Body: `{"reason":"approved by phone with the on-call engineer"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tokens, users := auth.NewMemStore(), user.NewMemStore()
			admin, err := user.New("ops-admin", "a long enough password", user.RoleAdmin)
			if err != nil {
				t.Fatalf("user.New() error = %v", err)
			}
			if err := users.Save(ctx, admin); err != nil {
				t.Fatalf("users.Save() error = %v", err)
			}
			plain, tok, err := auth.New("laptop")
			if err != nil {
				t.Fatalf("auth.New() error = %v", err)
			}
			tok.UserID = admin.ID
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("tokens.Save() error = %v", err)
			}
			store, audits := run.NewMemStore(), audit.NewMemStore()
			runner := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 0}, nil
				},
			)
			d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithAudits(audits),
				dispatch.WithDecisions(decision.NewMemStore()), dispatch.WithNoJanitor())
			t.Cleanup(d.Close)
			held := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
				Queue: "served-by-nobody", Status: run.StatusPendingApproval, CreatedAt: time.Now()}
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("store.Save() error = %v", err)
			}
			handler := New(store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users),
				WithAudit(audits), WithApprover(d)).Handler()
			path := "/v1/runs/" + held.ID + test.Path
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.Body))
			req.Header.Set("Authorization", "Bearer "+plain)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST %s = %d %s, want 200", path, rec.Code, rec.Body.String())
			}
			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var entry *audit.Entry
			for _, e := range chain {
				if e.Method == http.MethodPost && e.Path == path {
					entry = e
				}
			}
			if entry == nil || entry.ContentDigest == "" {
				t.Fatalf("the request was not recorded with a fingerprint: %+v", entry)
			}
			if audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, []byte(test.Body)) {
				t.Error("the request fingerprint commits the reason as typed")
			}
			if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce,
				withholdReasonText([]byte(test.Body))) {
				t.Error("the request fingerprint does not commit the body with the reason withheld")
			}
		})
	}
}
