package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/mcp"
)

// TestMCPRefusesToStartOnAnAdminToken proves the mcp command probes the token's authority before it
// serves anything, and refuses when the probe says the token can administer the install. An agent on
// an admin token could approve its own runs, so the refusal is the whole point of the check; a
// command that started anyway would hand that authority to the agent with only a doc comment
// standing in the way.
func TestMCPRefusesToStartOnAnAdminToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the token authority the server reports.
		Name string
		// Status is what the account endpoint answers the probe with.
		Status int
		// WantError is a fragment the returned error must carry.
		WantError string
	}{{ // Test 0: Listing accounts succeeds, so the token is an admin token and the command refuses.
		Name: "admin token", Status: http.StatusOK,
		WantError: "refusing to serve an agent on an admin token",
	}, { // Test 1: A rejected token is reported as itself, not read as either verdict.
		Name: "rejected token", Status: http.StatusUnauthorized,
		WantError: "the token was rejected by the server",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			// Not parallel: the mcp command reads package-level flag variables.
			var probes atomic.Int64
			var probedPath, probedAuth atomic.Value
			probedPath.Store("")
			probedAuth.Store("")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				probedPath.Store(r.URL.Path)
				probedAuth.Store(r.Header.Get("Authorization"))
				w.WriteHeader(test.Status)
			}))
			defer srv.Close()

			mcpServer, mcpToken = srv.URL, "st_agenttoken"
			mcpTimeout, mcpAllowAdminToken = 5*time.Second, false
			t.Cleanup(func() {
				mcpServer, mcpToken, mcpTimeout, mcpAllowAdminToken = "", "", 0, false
			})

			err := runMCP(testCommand(), nil)
			if err == nil {
				t.Fatalf("test %d: runMCP() error = nil, want it to stop before serving", testNum)
			}
			if !strings.Contains(err.Error(), test.WantError) {
				t.Fatalf("test %d: runMCP() error = %q, want it to carry %q",
					testNum, err, test.WantError)
			}
			// The probe is the side effect that proves the authority check ran rather than the
			// command failing for some other reason on its way to serving.
			if got := probes.Load(); got != 1 {
				t.Errorf("test %d: server saw %d request(s), want exactly the authority probe", testNum, got)
			}
			if got := probedPath.Load().(string); got != "/v1/users" {
				t.Errorf("test %d: probed %q, want the account endpoint", testNum, got)
			}
			if got := probedAuth.Load().(string); got != "Bearer st_agenttoken" {
				t.Errorf("test %d: probe authorization = %q, want the agent's token", testNum, got)
			}
		})
	}
}

// errProbeFailed stands in for an authority probe that failed for a reason other than the token
// being an admin token, such as an unreachable server or a rejected token.
var errProbeFailed = errors.New("the token was rejected by the server")

// TestRefuseAdminAuthority pins what the authority probe's answer turns into, including the one
// thing that gets past the refusal.
//
// The long help stated the refusal flatly and never mentioned --allow-admin-token, which overrides
// exactly that, so the security claim a reader took away was stronger than the one the code makes.
// The override is real and deliberate, and it warns on every start, so both halves are pinned here:
// the refusal is the default, and the flag is the only way past it.
func TestRefuseAdminAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is what the authority probe answered.
		In error
		// Want is the error that must come back, matched with errors.Is. Nil means either nothing
		// comes back or the refusal is matched by its text instead.
		Want error
		// Name labels the probe's answer.
		Name string
		// WantErrorHas is text the returned error must carry.
		WantErrorHas []string
		// WantWarn is text the warning writer must receive, empty when it must stay untouched.
		WantWarn string
		// Allow is --allow-admin-token.
		Allow bool
		// WantRefused says whether an error must come back at all.
		WantRefused bool
	}{{ // Test 0: The probe passed, so the token cannot administer the install and serving proceeds.
		Name: "an operator-bound token",
	}, { // Test 1: The probe itself failed, which is neither verdict and is reported as itself.
		Name: "a probe that failed", In: fmt.Errorf("probe: %w", errProbeFailed),
		Want: errProbeFailed, WantRefused: true,
		WantErrorHas: []string{"the token was rejected by the server"},
	}, { // Test 2: An admin token with the override off, which is the default an agent gets. The
		// refusal names the override, since a refusal that hides its own escape hatch reads as
		// stronger than it is.
		Name: "an admin token", In: fmt.Errorf("probe: %w", mcp.ErrAdminToken), WantRefused: true,
		WantErrorHas: []string{
			"refusing to serve an agent on an admin token", "switchtender token new --user",
			"--allow-admin-token",
		},
	}, { // Test 3: An admin token with the override on. It serves, and it says so, because an
		// install running an agent on a token that can approve its own work should be told at every
		// start rather than once when somebody typed the flag.
		Name: "an admin token with the override", In: fmt.Errorf("probe: %w", mcp.ErrAdminToken),
		Allow: true, WantWarn: "serving an agent on an admin token, which can approve its own runs",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var warn bytes.Buffer
			err := refuseAdminAuthority(test.In, test.Allow, &warn)
			if test.WantRefused && err == nil {
				t.Fatal("refuseAdminAuthority() error = nil, want it to stop before serving")
			}
			if !test.WantRefused && err != nil {
				t.Fatalf("refuseAdminAuthority() error = %v, want it to serve", err)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("refuseAdminAuthority() error = %v, want it to carry %v", err, test.Want)
			}
			for _, want := range test.WantErrorHas {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("refuseAdminAuthority() error = %v, want it to name %q", err, want)
				}
			}
			if test.WantWarn == "" {
				if warn.Len() != 0 {
					t.Errorf("nothing should have been warned about, got %q", warn.String())
				}
				return
			}
			if !strings.Contains(warn.String(), test.WantWarn) {
				t.Errorf("warning = %q, want it to name %q", warn.String(), test.WantWarn)
			}
		})
	}
}

// TestMCPHelpNamesTheOverrideItRefusesWith holds the long help against the code it describes.
//
// It ended on "The command refuses to start on an admin token" and stopped there, while
// --allow-admin-token sat in the flag list overriding exactly that. A security claim stated without
// its override is the kind an evaluator punctures in a minute, and the claim survives being stated
// accurately: the refusal is still the default, the override still warns, and nothing else turns
// the check off.
func TestMCPHelpNamesTheOverrideItRefusesWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels what the help must say.
		Name string
		// Want is text the long help must carry.
		Want string
	}{{ // Test 0: The refusal itself, which is the claim being made.
		Name: "the refusal", Want: "refuses to start on an admin token",
	}, { // Test 1: The flag that overrides it, named in the same paragraph.
		Name: "the override", Want: "--allow-admin-token",
	}, { // Test 2: That the override is not silent, which is what keeps the default meaningful.
		Name: "the warning", Want: "prints a warning",
	}, { // Test 3: That nothing else turns the check off, so a reader does not go looking.
		Name: "no other way past", Want: "Nothing else turns the check off",
	}}
	// The help is wrapped to fit a terminal, so a sentence under test is split across lines wherever
	// it happens to fall. The words are what is being checked, not where the wrap put them.
	unwrapped := strings.Join(strings.Fields(mcpCmd.Long), " ")
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(unwrapped, test.Want) {
				t.Errorf("test %d: the mcp long help does not say %q", testNum, test.Want)
			}
		})
	}
	if mcpCmd.Flags().Lookup("allow-admin-token") == nil {
		t.Error("the help names --allow-admin-token and the command does not register it")
	}
}
