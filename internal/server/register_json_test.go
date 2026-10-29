package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// registerPeriod is the query naming a period explicitly, so a test reads the same window whatever
// the clock says when it runs.
const registerPeriod = "from=2026-08-03&to=2026-08-04"

// recordingRegister is a change register that records what the handler asked it for and answers
// with a fixed document and value. It knows nothing about collecting a register, so a test using it
// is about the handler: its gates, its period, and the form it answers in.
type recordingRegister struct {
	// mu guards the recorded fields.
	mu sync.Mutex
	// src is the source of the last request.
	src paid.RegisterSource
	// period is the period of the last request.
	period paid.RegisterPeriod
	// calls names each method called, in order.
	calls []string
	// err, when set, is returned by Render and Build.
	err error
}

// NewEmitter is not reached by the handler.
func (r *recordingRegister) NewEmitter(paid.EmitterConfig) (paid.Emitter, error) {
	panic("the register handler starts no emitter")
}

// Render records the request and returns a fixed document.
func (r *recordingRegister) Render(_ context.Context, src paid.RegisterSource,
	p paid.RegisterPeriod) ([]byte, error) {
	r.record("render", src, p)
	if r.err != nil {
		return nil, r.err
	}
	return []byte("<html>register</html>"), nil
}

// Build records the request and returns a fixed value.
func (r *recordingRegister) Build(_ context.Context, src paid.RegisterSource,
	p paid.RegisterPeriod) (any, error) {
	r.record("build", src, p)
	if r.err != nil {
		return nil, r.err
	}
	return map[string]string{"status": "verified"}, nil
}

// record keeps one request.
func (r *recordingRegister) record(call string, src paid.RegisterSource, p paid.RegisterPeriod) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	r.src, r.period = src, p
}

// last returns the calls made and the last request.
func (r *recordingRegister) last() ([]string, paid.RegisterSource, paid.RegisterPeriod) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...), r.src, r.period
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

// TestRegisterHandlerHandsThePeriodToTheRegister pins what the handler passes the paid register:
// the period the caller named, the install's stores and identity, and the binder that holds each
// approval to its seal when the install has a key. It pins which form answers too: the document by
// default and the data when format=json is asked for.
func TestRegisterHandlerHandsThePeriodToTheRegister(t *testing.T) {
	t.Parallel()
	key, err := binding.DeriveKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	tests := []struct {
		// Query is the request's query string.
		Query string
		// Key is the install's binding key, nil for an install that holds none.
		Key *binding.Key
		// WantCall is the register method the form reaches.
		WantCall string
		// WantType is the response's content type prefix.
		WantType string
	}{{ // Test 0: The document, with no binding key, so nothing is held to a seal.
		Query: registerPeriod, WantCall: "render", WantType: "text/html",
	}, { // Test 1: The data form, with a key, so each approval is held to its seal.
		Query: registerPeriod + "&format=json", Key: key, WantCall: "build",
		WantType: "application/json",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits := run.NewMemStore(), audit.NewMemStore()
			reg := &recordingRegister{}
			opts := []Option{WithAudit(audits), WithChangeRegister(reg)}
			if test.Key != nil {
				opts = append(opts, WithBinder(test.Key))
			}
			rec := getRegister(New(runs, &fakeSubmitter{}, zap.NewNop(), opts...).Handler(),
				test.Query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("register = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, test.WantType) {
				t.Errorf("content type = %q, want %s", got, test.WantType)
			}
			calls, src, p := reg.last()
			if diff := cmp.Diff([]string{test.WantCall}, calls); diff != "" {
				t.Errorf("register calls (-want +got):\n%s", diff)
			}
			from := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
			if !p.From.Equal(from) || !p.To.Equal(from.AddDate(0, 0, 1)) {
				t.Errorf("period = %s to %s, want the day the query named", p.From, p.To)
			}
			if p.Now.IsZero() {
				t.Error("the register was handed no collection time")
			}
			if src.Runs != runs || src.Audits != audits {
				t.Error("the register was not handed the install's stores")
			}
			if (src.Binder != nil) != (test.Key != nil) {
				t.Errorf("binder present = %t, want %t", src.Binder != nil, test.Key != nil)
			}
		})
	}
}

// TestRegisterHandlerRefusesWhereTheRegisterDoes pins how a refusal reads. A Community build's
// register refuses naming Team, which the caller is told as a 403 in both forms, where any other
// failure is the server's and says nothing of the cause.
func TestRegisterHandlerRefusesWhereTheRegisterDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Err is what the register returns.
		Err error
		// WantStatus is the response code for both forms.
		WantStatus int
		// WantText is a phrase the body carries.
		WantText string
	}{{ // Test 0: The Community stub names the tier and the price list.
		Name: "community", Err: paid.Requires(license.FeatureRegister), WantStatus: http.StatusForbidden,
		WantText: "switchtender.com/pricing",
	}, { // Test 1: A store fault is the server's, and its text stays in the log.
		Name: "fault", Err: errors.New("disk on fire"), WantStatus: http.StatusInternalServerError,
		WantText: "could not",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			reg := &recordingRegister{err: test.Err}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithAudit(audit.NewMemStore()), WithChangeRegister(reg)).Handler()
			for _, query := range []string{registerPeriod, registerPeriod + "&format=json"} {
				rec := getRegister(handler, query, "")
				if rec.Code != test.WantStatus {
					t.Errorf("%s: register %q = %d, want %d: %s", test.Name, query, rec.Code,
						test.WantStatus, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), test.WantText) {
					t.Errorf("%s: register %q body %q, want it to carry %q", test.Name, query,
						rec.Body.String(), test.WantText)
				}
				if strings.Contains(rec.Body.String(), "disk on fire") {
					t.Errorf("%s: a store fault's text reached the caller", test.Name)
				}
			}
		})
	}
}

// TestCommunityBuildRefusesTheRegisterNamingTeam pins the wall on the HTTP surface. This test
// binary carries no paid module, so the server's default register is the Community stub, and even
// a Team license reads a refusal naming Team rather than a register.
func TestCommunityBuildRefusesTheRegisterNamingTeam(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithAudit(audit.NewMemStore())).Handler()
	for _, query := range []string{registerPeriod, registerPeriod + "&format=json"} {
		rec := getRegister(handler, query, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Team license") {
			t.Errorf("register %q on Community = %d %s, want 403 naming Team", query, rec.Code,
				rec.Body.String())
		}
	}
}

// TestRegisterJSONHoldsToAdmin pins that the JSON form takes the audit trail's role on an install
// that signs people in: nobody reads it anonymously, a viewer is refused, and an admin reads it.
// The register page is a public shell like every page, so this gate is all that stands between the
// register and a reader who should not have it.
func TestRegisterJSONHoldsToAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
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
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithAudit(audit.NewMemStore()),
		WithTokens(tokens), WithUsers(users), WithChangeRegister(&recordingRegister{})).Handler()

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
