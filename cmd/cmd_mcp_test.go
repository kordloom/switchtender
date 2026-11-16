package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	"github.com/kordloom/switchtender/internal/mcp"
)

const (
	// mcpNotAgentLine is the one line the command refuses a token that is not an agent's with.
	mcpNotAgentLine = "refusing to serve: the token is not an agent token: mint one with " +
		"\"switchtender token new --user <account> --agent\""
	// mcpNoAccountsLine is the one line the command refuses a server with no accounts with.
	mcpNoAccountsLine = "refusing to serve: the server has no accounts, so nothing an agent asks " +
		"for can be held there: turn accounts on first with \"switchtender user new\""
)

// TestMCPStartsOnlyForAnAgentToken proves the mcp command asks the server who the token is before
// it serves anything, starts for an agent token, and refuses every other answer. The hold on an
// agent's runs applies to an agent token and to no other, so a command that started on a person's
// token would send an agent's runs through under that person's name with no hold.
func TestMCPStartsOnlyForAnAgentToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Want is the error that must come back, matched with errors.Is. Nil with an empty
		// WantText means the command serves.
		Want error
		// Name labels what the server says the token is.
		Name string
		// Body is what the server answers about the caller, in the shape GET /v1/auth/me writes.
		Body string
		// WantText is the whole refusal, empty when the command serves.
		WantText string
		// Status is the status the server answers the probe with.
		Status int
	}{{ // Test 0: A token minted with --agent is the one credential the command starts for.
		Name: "agent token", Status: http.StatusOK,
		Body: `{"name":"agent-bot","account":"dev-lead","role":"operator","actor_type":"agent"}`,
	}, { // Test 1: An operator's own token gets no agent hold, so the command refuses it.
		Name: "operator token", Status: http.StatusOK, Want: mcp.ErrNotAgentToken,
		Body:     `{"name":"deploy","account":"dev-lead","role":"operator","actor_type":"token"}`,
		WantText: mcpNotAgentLine,
	}, { // Test 2: An admin token is refused with the same line.
		Name: "admin token", Status: http.StatusOK, Want: mcp.ErrNotAgentToken,
		Body:     `{"name":"initial-admin","role":"admin","actor_type":"token"}`,
		WantText: mcpNotAgentLine,
	}, { // Test 3: A person's browser session is refused with the same line.
		Name: "session", Status: http.StatusOK, Want: mcp.ErrNotAgentToken,
		Body:     `{"name":"dev-lead","account":"dev-lead","role":"admin","actor_type":"session"}`,
		WantText: mcpNotAgentLine,
	}, { // Test 4: A server with no accounts holds nothing, and the refusal says to turn them on.
		Name: "open install", Status: http.StatusOK, Want: mcp.ErrNoAccounts,
		Body: `{"open":true}`, WantText: mcpNoAccountsLine,
	}, { // Test 5: A rejected token is reported as itself, not read as either refusal.
		Name: "rejected token", Status: http.StatusUnauthorized,
		WantText: "the token was rejected by the server",
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
				probedPath.Store(r.Method + " " + r.URL.Path)
				probedAuth.Store(r.Header.Get("Authorization"))
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(test.Body))
			}))
			defer srv.Close()

			setString(t, &mcpServer, srv.URL)
			setString(t, &mcpToken, "st_agenttoken")
			setDuration(t, &mcpTimeout, 5*time.Second)

			// One tools/list request, which the server answers without calling the API, so a reply
			// on out is proof the command got as far as serving.
			in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
			var out, progress bytes.Buffer
			err := serveMCP(context.Background(), in, &out, &progress)

			if test.WantText == "" {
				if err != nil {
					t.Fatalf("test %d: serveMCP() error = %v, want it to serve", testNum, err)
				}
				if !strings.Contains(out.String(), `"propose_run"`) {
					t.Errorf("test %d: reply = %q, want the tool list", testNum, out.String())
				}
				if !strings.Contains(progress.String(), "mcp: serving 7 tools against "+srv.URL) {
					t.Errorf("test %d: progress = %q, want the serving line",
						testNum, progress.String())
				}
			} else {
				if err == nil {
					t.Fatalf("test %d: serveMCP() error = nil, want it to stop before serving",
						testNum)
				}
				if test.Want != nil && !errors.Is(err, test.Want) {
					t.Errorf("test %d: serveMCP() error = %v, want it to carry %v",
						testNum, err, test.Want)
				}
				if diff := cmp.Diff(test.WantText, err.Error()); diff != "" {
					t.Errorf("test %d: refusal mismatch (-want +got):\n%s", testNum, diff)
				}
				if out.Len() != 0 || progress.Len() != 0 {
					t.Errorf("test %d: a refused command wrote %q and %q, want nothing served",
						testNum, out.String(), progress.String())
				}
			}
			// The probe is the side effect that proves the token check ran rather than the command
			// stopping for some other reason on its way to serving.
			if got := probes.Load(); got != 1 {
				t.Errorf("test %d: server saw %d request(s), want exactly the probe", testNum, got)
			}
			if got := probedPath.Load().(string); got != "GET /v1/auth/me" {
				t.Errorf("test %d: probed %q, want GET /v1/auth/me", testNum, got)
			}
			if got := probedAuth.Load().(string); got != "Bearer st_agenttoken" {
				t.Errorf("test %d: probe authorization = %q, want the agent's token", testNum, got)
			}
		})
	}
}

// errProbeFailed stands in for a token check that failed for a reason other than what the token
// is, such as an unreachable server or a rejected token.
var errProbeFailed = errors.New("the token was rejected by the server")

// TestMCPAgentTokenRefusal pins what the startup check's answer turns into. Each refusal is one
// line that says what to do next, and a check that could not be made is reported as itself.
func TestMCPAgentTokenRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is what the startup check answered.
		In error
		// Want is the error that must come back, matched with errors.Is. Nil means nothing comes
		// back and the command serves.
		Want error
		// Name labels the check's answer.
		Name string
		// WantText is the whole message of the returned error.
		WantText string
	}{{ // Test 0: The check passed, so the token is an agent token and serving proceeds.
		Name: "an agent token",
	}, { // Test 1: The check itself failed, which is neither refusal and is reported as itself.
		Name: "a check that failed", In: fmt.Errorf("probe: %w", errProbeFailed),
		Want: errProbeFailed, WantText: "probe: the token was rejected by the server",
	}, { // Test 2: A token that is not an agent's, with the command that mints one.
		Name: "a token that is not an agent's", In: mcp.ErrNotAgentToken,
		Want: mcp.ErrNotAgentToken, WantText: mcpNotAgentLine,
	}, { // Test 3: A server with no accounts, with what has to be on first.
		Name: "a server with no accounts", In: mcp.ErrNoAccounts,
		Want: mcp.ErrNoAccounts, WantText: mcpNoAccountsLine,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := agentTokenRefusal(test.In)
			if test.Want == nil {
				if err != nil {
					t.Fatalf("agentTokenRefusal() error = %v, want it to serve", err)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("agentTokenRefusal() error = %v, want it to carry %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantText, err.Error()); diff != "" {
				t.Errorf("refusal mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("refusal = %q, want one line", err.Error())
			}
		})
	}
}

// TestMCPHelpSaysWhichTokenItServes holds the long help against the code it describes. The help
// states the condition the command starts under, both refusals, that no flag overrides them, and
// how far the refusal reaches, and the flag set is pinned so an override cannot be added without
// this test changing.
func TestMCPHelpSaysWhichTokenItServes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels what the help must say.
		Name string
		// Want is text the long help must carry.
		Want string
	}{{ // Test 0: The condition the command starts under.
		Name: "the condition", Want: "starts only for an agent token",
	}, { // Test 1: How to mint the token it starts for.
		Name: "the mint command", Want: `"switchtender token new --user <account> --agent"`,
	}, { // Test 2: That every other token is refused, an admin's and an operator's alike.
		Name: "the refusal", Want: "Any other token is refused, an admin's and an operator's alike",
	}, { // Test 3: That a server with no accounts is refused, and why.
		Name: "no accounts", Want: "A server with no accounts is refused too: nothing can be held",
	}, { // Test 4: That nothing turns the check off, so a reader does not go looking.
		Name: "no override", Want: "No flag turns the check off",
	}, { // Test 5: How far the refusal reaches, so it is not read as a property of the server.
		Name: "the reach", Want: "The refusal covers this command, and the server cannot tell a " +
			"person from a program holding that person's token",
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
	var got []string
	mcpCmd.Flags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)
	want := []string{"allow-adhoc", "server", "timeout", "token"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the mcp flag set changed (-want +got):\n%s\nThe help says no flag turns the "+
			"token check off, so a new flag has to be checked against that.", diff)
	}
}
