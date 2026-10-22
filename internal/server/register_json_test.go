package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/user"
)

// registerBase is the instant the seeded register periods are built around.
var registerBase = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// registerPeriod is the query naming the seeded period explicitly, so the two forms of one register
// read the same window whatever the clock says when the test runs.
const registerPeriod = "from=2026-08-03&to=2026-08-04"

// registerSecret is the password the seeded script assigns inline, which neither form may carry.
const registerSecret = "hunter2secret"

// seedRegisterKinds stores one run of every kind the register describes differently: approved,
// rejected, held, a dry run that is not change free, a failure, a run with no actor, and a script
// with an inline secret whose actor carries markup. It also records the outcome of a run the store
// no longer holds, so the period reads as pruned.
func seedRegisterKinds(t *testing.T) (run.Store, audit.Store) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	at := func(minutes int) time.Time { return registerBase.Add(time.Duration(minutes) * time.Minute) }
	for _, r := range []*run.Run{
		{ID: "run_approved", Playbook: "site.yml", Status: run.StatusSucceeded, Actor: "alice",
			Source: "template", SourceID: "tpl_web", CreatedAt: at(0)},
		{ID: "run_rejected", Tool: "terraform", Command: "infra/network", Status: run.StatusRejected,
			Actor: "alice", CreatedAt: at(1)},
		{ID: "run_held", Playbook: "db.yml", Status: run.StatusPendingApproval, Actor: "agent-7",
			HeldByPolicy: "requested by an agent, held by default", CreatedAt: at(2)},
		{ID: "run_dry", Tool: "plugin-tool", Command: "plan everything", DryRun: true,
			Status: run.StatusSucceeded, Actor: "alice", CreatedAt: at(3)},
		{ID: "run_failed", Playbook: "patch.yml", Status: run.StatusFailed, Actor: "alice",
			CreatedAt: at(4)},
		{ID: "run_noactor", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: at(5)},
		{ID: "run_secret", Tool: run.ToolBash, Status: run.StatusSucceeded,
			Command: "export DB_PASSWORD=" + registerSecret + "\npsql -c 'select 1'",
			Actor:   `dev "ops" <a&b>`, CreatedAt: at(6)},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	for _, e := range []struct{ Actor, Method, Path string }{
		{"bob", audit.MethodDecision, "/runs/run_approved/decision/approved"},
		{"carol", audit.MethodDecision, "/runs/run_rejected/decision/rejected"},
		{"system:dispatcher", audit.MethodRun, "/runs/run_gone/outcome/succeeded"},
	} {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: at(10), Actor: e.Actor, Method: e.Method, Path: e.Path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return runs, audits
}

// seedRegisterPastTheCap stores one change more than a register carries, so both forms are cut.
func seedRegisterPastTheCap(t *testing.T) (run.Store, audit.Store) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	for i := range dossier.MaxRegisterRuns + 1 {
		if err := runs.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_%05d", i), Playbook: "site.yml", Status: run.StatusSucceeded,
			Actor: "alice", CreatedAt: registerBase.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return runs, audit.NewMemStore()
}

// getRegister asks handler for the register at query with an optional bearer token.
func getRegister(handler http.Handler, query, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/register?"+query, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// decodeRegister reads the JSON form of the register, failing the test when it is not one.
func decodeRegister(t *testing.T, rec *httptest.ResponseRecorder) dossier.Register {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("register JSON = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("register JSON content type = %q, want application/json", got)
	}
	var reg dossier.Register
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register JSON does not decode: %v", err)
	}
	return reg
}

// registerRowPattern and registerCellPattern cut the document's change table into rows and cells,
// and registerTagPattern strips the markup inside a cell, leaving the text a reader sees.
var (
	registerRowPattern  = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
	registerCellPattern = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	registerTagPattern  = regexp.MustCompile(`<[^>]+>`)
)

// documentRows returns the text of every cell in the document's change table, row by row, in the
// order the document lists them.
func documentRows(t *testing.T, doc string) [][]string {
	t.Helper()
	_, table, ok := strings.Cut(doc, `id="register-table"`)
	if !ok {
		t.Fatal("the register document carries no change table")
	}
	_, body, ok := strings.Cut(table, "<tbody>")
	if !ok {
		t.Fatal("the change table has no body")
	}
	body, _, _ = strings.Cut(body, "</tbody>")
	var rows [][]string
	for _, tr := range registerRowPattern.FindAllStringSubmatch(body, -1) {
		var cells []string
		for _, td := range registerCellPattern.FindAllStringSubmatch(tr[1], -1) {
			text := registerTagPattern.ReplaceAllString(td[1], "")
			cells = append(cells, strings.TrimSpace(html.UnescapeString(text)))
		}
		rows = append(rows, cells)
	}
	return rows
}

// documentCells returns what the document prints for one JSON row in the five columns the parity
// test compares: run, change, actor, decision, and outcome. It applies the document's own words for
// an empty value, so a row reads the same whichever form it came from.
func documentCells(row dossier.RegisterRow) []string {
	change := row.Change
	if row.DryRun {
		change += " dry run"
	}
	if row.NotChangeFree {
		change += " not change free"
	}
	actor := row.Actor
	if actor == "" {
		actor = "none recorded"
	}
	decision := "—"
	if row.Decision != "" {
		decision = fmt.Sprintf("%s #%d", row.Decision, row.DecisionSeq)
	}
	return []string{row.Run, change, actor, decision, row.Outcome}
}

// TestRegisterJSONAndDocumentAgree pins that the register page and the downloaded document show the
// same changes in the same words, row for row and in order.
//
// The web interface renders the register from its JSON form and the auditor receives the HTML
// document. If the two were built by separate code they would drift, and the page a reviewer
// checked would stop being the document they were sent. Both forms come from one builder; this
// holds them to it across every kind of row the register describes differently, and across the
// notices a pruned or truncated period carries.
func TestRegisterJSONAndDocumentAgree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Seed       func(t *testing.T) (run.Store, audit.Store)
		Query      string
		WantRows   int
		WantNotice func(reg dossier.Register) string
	}{{ // Test 0: Every kind of row, over a period retention thinned.
		Name: "every kind", Seed: seedRegisterKinds, Query: registerPeriod, WantRows: 7,
		WantNotice: func(reg dossier.Register) string { return reg.PrunedText },
	}, { // Test 1: A period past the cap, cut in both forms at the same row.
		Name: "past the cap", Seed: seedRegisterPastTheCap, Query: registerPeriod,
		WantRows:   dossier.MaxRegisterRuns,
		WantNotice: func(reg dossier.Register) string { return reg.TruncatedText },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits := test.Seed(t)
			handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithAudit(audits)).Handler()

			reg := decodeRegister(t, getRegister(handler, test.Query+"&format=json", ""))
			rec := getRegister(handler, test.Query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: register document = %d, want 200", test.Name, rec.Code)
			}
			doc := rec.Body.String()
			rows := documentRows(t, doc)
			if len(reg.Rows) != test.WantRows || len(rows) != test.WantRows {
				t.Fatalf("%s: JSON carries %d rows and the document %d, want %d in both", test.Name,
					len(reg.Rows), len(rows), test.WantRows)
			}
			for i, row := range reg.Rows {
				got := rows[i]
				if len(got) != 9 {
					t.Fatalf("%s: document row %d has %d cells, want 9", test.Name, i, len(got))
				}
				printed := []string{got[1], got[2], got[3], got[7], got[8]}
				if diff := cmp.Diff(documentCells(row), printed, cmpopts.EquateEmpty()); diff != "" {
					t.Fatalf("%s: row %d differs between the forms (-json +document):\n%s",
						test.Name, i, diff)
				}
			}
			notice := test.WantNotice(reg)
			if notice == "" {
				t.Fatalf("%s: the JSON form carries no notice for this period", test.Name)
			}
			if !strings.Contains(doc, html.EscapeString(notice)) {
				t.Errorf("%s: the document does not carry the JSON form's notice %q", test.Name, notice)
			}
		})
	}
}

// TestRegisterHoldsApprovalsToTheirSeals pins that both forms of the register state the seal check
// on every approval they list. Dropping the check from the handler printed each approval as plain,
// which reads to an auditor as checked and holding, and no test noticed. An install with no binding
// key marks the approval unchecked, and one with a key finds that the row the approval released
// carries no seal, which also proves the server's key reaches the check.
func TestRegisterHoldsApprovalsToTheirSeals(t *testing.T) {
	t.Parallel()
	key, err := binding.DeriveKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	tests := []struct {
		// Key is the install's binding key, nil for an install that holds none.
		Key *binding.Key
		// WantDecision is the approved row's decision text.
		WantDecision string
	}{{ // Test 0: No key marks the approval unchecked.
		WantDecision: "Approved by bob, seal unchecked",
	}, { // Test 1: A key finds the released row carries no seal.
		Key: key, WantDecision: "Approved by bob, unsealed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits := seedRegisterKinds(t)
			opts := []Option{WithAudit(audits)}
			if test.Key != nil {
				opts = append(opts, WithBinder(test.Key))
			}
			handler := New(runs, &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			reg := decodeRegister(t, getRegister(handler, registerPeriod+"&format=json", ""))
			var got string
			for _, row := range reg.Rows {
				if row.Run == "run_approved" {
					got = row.Decision
				}
			}
			if got != test.WantDecision {
				t.Errorf("JSON decision = %q, want %q", got, test.WantDecision)
			}
			rec := getRegister(handler, registerPeriod, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("register document = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), html.EscapeString(test.WantDecision)) {
				t.Errorf("the register document does not read %q", test.WantDecision)
			}
		})
	}
}

// TestRegisterJSONRedactsAnInlineSecret pins that the JSON form scrubs a secret a run's script
// assigned, as the document does. The register page renders this form, so a secret that survived
// here would be shown on screen and copied into every export the page offers.
func TestRegisterJSONRedactsAnInlineSecret(t *testing.T) {
	t.Parallel()
	runs, audits := seedRegisterKinds(t)
	handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithAudit(audits)).Handler()
	rec := getRegister(handler, registerPeriod+"&format=json", "")
	reg := decodeRegister(t, rec)
	if strings.Contains(rec.Body.String(), registerSecret) {
		t.Fatal("the JSON register publishes a secret from a run's script")
	}
	for _, row := range reg.Rows {
		if row.Run != "run_secret" {
			continue
		}
		if !strings.Contains(row.Change, scrub.Marker) {
			t.Errorf("the scripted run's change = %q, want the secret replaced by %q", row.Change,
				scrub.Marker)
		}
		return
	}
	t.Fatal("the JSON register does not list the scripted run")
}

// TestRegisterJSONFollowsTheLicense pins that the JSON form is gated exactly as the document is: a
// Team install reads the rows, and a Community install is refused with the sentence that names Team
// and the pricing page, which is what the register page turns into its offer.
//
// Deliberately not parallel: the license is process state, and asCommunity swaps it out.
func TestRegisterJSONFollowsTheLicense(t *testing.T) {
	tests := []struct {
		Name       string
		Community  bool
		WantStatus int
		WantRuns   []string
		WantText   []string
	}{{ // Test 0: The package runs under Team, so the rows come back.
		Name: "team", WantStatus: http.StatusOK,
		WantRuns: []string{"run_approved", "run_rejected", "run_held", "run_dry", "run_failed",
			"run_noactor", "run_secret"},
	}, { // Test 1: Community is refused, and told what to buy and where.
		Name: "community", Community: true, WantStatus: http.StatusForbidden,
		WantText: []string{"Team license", "https://switchtender.com/pricing"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			runs, audits := seedRegisterKinds(t)
			handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithAudit(audits)).Handler()
			var rec *httptest.ResponseRecorder
			fetch := func() { rec = getRegister(handler, registerPeriod+"&format=json", "") }
			if test.Community {
				asCommunity(t, fetch)
			} else {
				fetch()
			}
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: register JSON = %d, want %d: %s", test.Name, rec.Code, test.WantStatus,
					rec.Body.String())
			}
			for _, want := range test.WantText {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("%s: the refusal does not carry %q: %s", test.Name, want,
						rec.Body.String())
				}
			}
			if test.WantRuns == nil {
				return
			}
			var got []string
			for _, row := range decodeRegister(t, rec).Rows {
				got = append(got, row.Run)
			}
			if diff := cmp.Diff(test.WantRuns, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: register rows (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestRegisterJSONHoldsToAdmin pins that the JSON form takes the audit trail's role on an install
// that signs people in: nobody reads it anonymously, a viewer is refused, and an admin reads it.
// The register page is a public shell like every page, so this gate is all that stands between the
// register and a reader who should not have it.
func TestRegisterJSONHoldsToAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs, audits := seedRegisterKinds(t)
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	mint := func(name string, role user.Role) string {
		u, err := user.New(name, "pw", role)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save() error = %v", err)
		}
		plain, tok, err := auth.New("t-" + name)
		if err != nil {
			t.Fatalf("auth.New() error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save() error = %v", err)
		}
		return plain
	}
	viewer := mint("reader", user.RoleViewer)
	admin := mint("boss", user.RoleAdmin)
	handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithAudit(audits), WithTokens(tokens),
		WithUsers(users)).Handler()

	tests := []struct {
		Name       string
		Bearer     string
		WantStatus int
	}{{ // Test 0: No token is refused as unauthenticated.
		Name: "anonymous", WantStatus: http.StatusUnauthorized,
	}, { // Test 1: A viewer is signed in but below the trail's role.
		Name: "viewer", Bearer: viewer, WantStatus: http.StatusForbidden,
	}, { // Test 2: An admin reads the register.
		Name: "admin", Bearer: admin, WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := getRegister(handler, registerPeriod+"&format=json", test.Bearer)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: register JSON = %d, want %d: %s", test.Name, rec.Code,
					test.WantStatus, rec.Body.String())
			}
		})
	}
}
