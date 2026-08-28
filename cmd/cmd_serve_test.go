package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenCountGuard covers the pre-bind decision about authentication. The count-error case is the
// fail-closed fix: an unreadable token store must refuse to start, where the former guard skipped the
// check and fell through to bind on the guess that no tokens existed. The bootstrap case is the
// other half: a public bind on an empty store used to be refused outright, which made the documented
// first command exit 1 on every fresh install.
func TestTokenCountGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Count        int
		CountErr     error
		ReadOnly     bool
		ExternalAuth bool
		Loopback     bool
		Addr         string
		WantPosture  servePosture
		WantErr      bool
	}{
		{ // Test 0: Tokens exist, so serving is allowed with no warning.
			Name: "tokens exist", Count: 3, Addr: "0.0.0.0:8080",
		},
		{ // Test 1: A count error fails closed and refuses to start, even on a public bind.
			Name: "count error public", Count: 0, CountErr: errors.New("db locked"),
			Addr: "0.0.0.0:8080", WantErr: true,
		},
		{ // Test 2: A count error fails closed even for a loopback bind.
			Name: "count error loopback", Count: 0, CountErr: errors.New("db locked"),
			Loopback: true, Addr: "127.0.0.1:8080", WantErr: true,
		},
		{ // Test 3: An empty readable store on a public bind with no other auth mints a token, so
			// the bind is authenticated from the first request and the documented command still works.
			Name: "empty public no auth", Count: 0, Addr: "0.0.0.0:8080",
			WantPosture: postureBootstrap,
		},
		{ // Test 4: An empty store on loopback is allowed with a warning.
			Name: "empty loopback", Count: 0, Loopback: true, Addr: "127.0.0.1:8080", WantPosture: postureWarn,
		},
		{ // Test 5: An empty store with external auth on a public bind serves with no warning,
			// because the API is not unauthenticated: an install configured for single sign-on
			// enforces from the first request. This case used to warn, which was the honest
			// reading of the old behavior, where the gate derived enforcement from the empty
			// token and account tables and served the whole API to anonymous callers as admin
			// until somebody happened to sign in.
			Name: "empty sso public", Count: 0, ExternalAuth: true, Addr: "0.0.0.0:8080",
		},
		{ // Test 6: An empty store in read-only mode on a public bind is allowed with a warning.
			Name: "empty read-only public", Count: 0, ReadOnly: true, Addr: "0.0.0.0:8080",
			WantPosture: postureWarn,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			posture, err := tokenCountGuard(test.Count, test.CountErr, test.ReadOnly,
				test.ExternalAuth, test.Loopback, test.Addr)
			if (err != nil) != test.WantErr {
				t.Errorf("%s: err = %v, want error: %v", test.Name, err, test.WantErr)
			}
			if posture != test.WantPosture {
				t.Errorf("%s: posture = %v, want %v", test.Name, posture, test.WantPosture)
			}
		})
	}
}

// TestIdentityDir proves the producer signing identity directory is derived safely for both database
// kinds. A SQLite file keeps its identity beside it. A postgres DSN has no filesystem home, so it
// falls back to a stable per-user directory rather than filepath.Dir of the DSN, which would be a
// cwd-relative junk directory whose name embeds the DSN's user:password@host.
func TestIdentityDir(t *testing.T) {
	// A SQLite file: identity lives in the directory beside it.
	got, err := identityDir("/var/lib/switchtender/demo.db")
	if err != nil {
		t.Fatalf("identityDir(sqlite) error = %v", err)
	}
	if got != "/var/lib/switchtender" {
		t.Errorf("identityDir(sqlite) = %q, want /var/lib/switchtender", got)
	}
	// A postgres DSN must not become a path and must not carry the credentials into a directory name.
	const dsn = "postgres://svc:hunter2@db.internal:5432/prod?sslmode=disable"
	got, err = identityDir(dsn)
	if err != nil {
		t.Fatalf("identityDir(dsn) error = %v", err)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "db.internal") ||
		strings.Contains(got, "postgres:") {
		t.Errorf("identityDir(dsn) = %q, leaks DSN content into the path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("switchtender", "identity")) {
		t.Errorf("identityDir(dsn) = %q, want a stable switchtender/identity directory", got)
	}
	// postgresql:// is the same case.
	g2, err := identityDir("postgresql://u:p@h/db")
	if err != nil {
		t.Fatalf("identityDir(postgresql) error = %v", err)
	}
	if g2 != got {
		t.Errorf("identityDir(postgresql) = %q, want the same stable dir as postgres:// %q", g2, got)
	}
}

// TestNotifySecretsPreferTheEnvironment pins that a third-party notification credential can be given
// through a channel a run cannot read.
//
// These were flag-only. A run executes as a child of this process under the same uid, so an
// operator-role user could read the server's argv from inside a bash run and lift the ntfy bearer,
// the Grafana API token, or the Twilio auth token, none of which their role grants. The environment
// is already closed against that: filterRunEnv drops every SWITCHTENDER_ variable before a run sees
// it. This checks the environment wins, that the flag still works where nobody cares, and that the
// help says which channel is the safe one.
func TestNotifySecretsPreferTheEnvironment(t *testing.T) {
	const fromEnv = "env-supplied-secret"
	const fromFlag = "flag-supplied-secret"

	t.Setenv("SWITCHTENDER_NOTIFY_NTFY_TOKEN", fromEnv)
	if got := notifySecret(fromFlag, "SWITCHTENDER_NOTIFY_NTFY_TOKEN"); got != fromEnv {
		t.Errorf("notifySecret = %q, want the environment value: the flag is visible in the process "+
			"list to any run this server starts", got)
	}

	// With nothing in the environment the flag still configures it.
	if got := notifySecret(fromFlag, "SWITCHTENDER_NOTIFY_UNSET_FOR_TEST"); got != fromFlag {
		t.Errorf("notifySecret = %q, want the flag value when the environment is empty", got)
	}

	// The variables live behind the same prefix filterRunEnv strips, which is what makes the
	// environment the safe channel rather than merely a different one.
	for _, name := range []string{
		"SWITCHTENDER_NOTIFY_NTFY_TOKEN",
		"SWITCHTENDER_NOTIFY_GRAFANA_TOKEN",
		"SWITCHTENDER_NOTIFY_TWILIO_TOKEN",
	} {
		if !strings.HasPrefix(name, "SWITCHTENDER_") {
			t.Errorf("%s is not behind the prefix a run's environment is filtered on", name)
		}
		flagName := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, "SWITCHTENDER_"), "_", "-"))
		f := serveCmd.Flags().Lookup(flagName)
		if f == nil {
			t.Errorf("no --%s flag to document %s against", flagName, name)
			continue
		}
		if !strings.Contains(f.Usage, name) {
			t.Errorf("--%s does not name %s, so an operator is not told which channel is safe",
				flagName, name)
		}
	}
}
