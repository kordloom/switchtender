package server

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRunMaskKeepsEveryOtherByte is the fact the run list's search rests on. The store narrows a
// search by the raw command, and the handler then matches what the caller is shown. That is a
// superset only if masking replaces a value and leaves every other byte alone, so the pieces of the
// masked text between markers must each appear in the raw command, in order.
func TestRunMaskKeepsEveryOtherByte(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantMasked bool
	}{ // Test 0: One assignment before a script.
		{"DB_PASSWORD=s3cretvalue ./x.sh", true},
		// Test 1: A YAML form inside a heredoc.
		{"cat <<EOF\napi_token: abc123def\nEOF\n./deploy.sh", true},
		// Test 2: Two assignments, one quoted.
		{`export TOKEN="t0k3n value" && SECRET_KEY=k3y ./run`, true},
		// Test 3: An assignment at the end with no tail.
		{"./x.sh --flag PASSWORD=hunter2", true},
		// Test 4: Mixed case names and a colon form.
		{"Password: Pa55 ; echo done", true},
		// Test 5: Nothing secret shaped, so nothing changes.
		{"ansible-playbook site.yml -l web", false},
		// Test 6: A value holding the marker's own letters.
		{"SECRET=[redacted]x ./y.sh", true},
		// Test 7: Several lines and tabs around assignments.
		{"\tAWS_SECRET_ACCESS_KEY=aaaa\n\tAWS_ACCESS_KEY_ID=bbbb\n\tmake", true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			masked := redactRunCommand(&run.Run{Command: test.In}).Command
			if got := masked != test.In; got != test.WantMasked {
				t.Fatalf("masked = %v, want %v: %q became %q", got, test.WantMasked, test.In, masked)
			}
			rest := test.In
			for _, piece := range strings.Split(masked, scrub.Marker) {
				i := strings.Index(rest, piece)
				if i < 0 {
					t.Fatalf("piece %q of the masked text %q is not in the raw command after the "+
						"pieces before it, so the mask changed a byte outside a value and the store's "+
						"raw narrowing is not a superset", piece, masked)
				}
				rest = rest[i+len(piece):]
			}
		})
	}
}

// searchStores returns a memory store and a SQLite store, so each search row runs on both.
func searchStores(t *testing.T) map[string]run.Store {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return map[string]run.Store{"memory": run.NewMemStore(), "sqlite": db.Runs()}
}

// agentCtx is the context an agent token's request carries.
func agentCtx() context.Context {
	return context.WithValue(context.Background(), actorKey{},
		Actor{UserID: "user_agent", Role: user.RoleOperator, Agent: true})
}

// TestListRunsSearchDoesNotMatchMaskedSecret is the audit's search leak. A caller below admin is
// shown the command with the secret masked, and a search matched the stored text, so whether a run
// came back answered whether its secret began with the letters searched for, one guess at a time.
func TestListRunsSearchDoesNotMatchMaskedSecret(t *testing.T) {
	t.Parallel()
	for name, store := range searchStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := store.Save(context.Background(), &run.Run{
				ID: "run_secret", Tool: "bash", Status: run.StatusSucceeded,
				Command: "DB_PASSWORD=s3cretvalue ./x.sh", CreatedAt: pagingBase,
			}); err != nil {
				t.Fatalf("save run: %v", err)
			}
			handler := listRunsHandler(store, &authorizer{}, zap.NewNop())
			callers := map[string]context.Context{
				"viewer": ctxActor("user_viewer", user.RoleViewer),
				"agent":  agentCtx(),
			}
			for who, ctx := range callers {
				tests := []struct {
					Query  string
					WantID []string
				}{ // Test 0: A prefix of the secret finds nothing.
					{"db_password=s3", nil},
					// Test 1: A wrong guess finds nothing, the same answer as a right one.
					{"db_password=zz", nil},
					// Test 2: A word no mask covers still finds the run.
					{"x.sh", []string{"run_secret"}},
					// Test 3: The name and the sign before the value still find it.
					{"db_password=", []string{"run_secret"}},
					// Test 4: The marker's own word is not text the run holds.
					{"redacted", nil},
					// Test 5: The secret alone finds nothing.
					{"s3cretvalue", nil},
					// Test 6: Text that straddles the mask finds nothing.
					{"value ./x", nil},
				}
				for testNum, test := range tests {
					resp := decodeRuns(t, listRunsRaw(t, handler, ctx, "?q="+url.QueryEscape(test.Query)))
					if diff := cmp.Diff(test.WantID, runIDs(resp.Runs), cmpopts.EquateEmpty()); diff != "" {
						t.Errorf("%s, test %d, q=%s (-want +got):\n%s", who, testNum, test.Query, diff)
					}
				}
			}
			// Admin control: an admin reads the command unmasked, so the secret's prefix finds it.
			resp := decodeRuns(t, listRunsRaw(t, handler, ctxActor("user_root", user.RoleAdmin),
				"?q=db_password=s3"))
			if diff := cmp.Diff([]string{"run_secret"}, runIDs(resp.Runs)); diff != "" {
				t.Errorf("admin search (-want +got):\n%s", diff)
			}
		})
	}
}

// TestListRunsSearchOracle builds two stores equal apart from one secret's value and sends a viewer
// the same probes against each. Every body must be equal, paging included, so no probe can tell the
// two values apart.
func TestListRunsSearchOracle(t *testing.T) {
	t.Parallel()
	build := func(secret string) run.Store {
		store := run.NewMemStore()
		for i := range 6 {
			cmd := fmt.Sprintf("./step%d.sh", i)
			if i%2 == 0 {
				cmd = "API_TOKEN=" + secret + " " + cmd
			}
			if err := store.Save(context.Background(), &run.Run{
				ID: fmt.Sprintf("run_%d", i), Tool: "bash", Status: run.StatusSucceeded, Command: cmd,
				CreatedAt: pagingBase.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatalf("save run: %v", err)
			}
		}
		return store
	}
	a := listRunsHandler(build("alpha111"), &authorizer{}, zap.NewNop())
	b := listRunsHandler(build("bravo222"), &authorizer{}, zap.NewNop())
	ctx := ctxActor("user_viewer", user.RoleViewer)
	var probes []string
	for _, q := range []string{"api_token=a", "api_token=b", "alpha", "bravo", "111", "222", "step",
		"api_token=", "a", "b"} {
		probes = append(probes, "?q="+url.QueryEscape(q))
		for k := range 4 {
			probes = append(probes, fmt.Sprintf("?q=%s&limit=1&offset=%d", url.QueryEscape(q), k))
		}
	}
	for _, q := range probes {
		if diff := cmp.Diff(string(listRunsRaw(t, a, ctx, q)), string(listRunsRaw(t, b, ctx, q))); diff != "" {
			t.Errorf("GET /v1/runs%s differs between two secrets, so a viewer can tell them apart "+
				"(-alpha +bravo):\n%s", q, diff)
		}
	}
}

// TestListRunsSearchRefusesTheMaskByte confirms a term holding the stand-in byte matches nothing for
// a caller below admin, so the byte cannot be used to find where a mask sits.
func TestListRunsSearchRefusesTheMaskByte(t *testing.T) {
	t.Parallel()
	if matchesShown(ctxActor("user_viewer", user.RoleViewer), &run.Run{
		Command: "DB_PASSWORD=s3cretvalue ./x.sh",
	}, "="+shownMaskByte) {
		t.Error("a term holding the mask byte matched the shown command")
	}
}
