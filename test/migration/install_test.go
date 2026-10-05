package migration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	_ "github.com/jackc/pgx/v5/stdlib" // Registers the pgx driver the own-database helper opens.

	"github.com/kordloom/switchtender/internal/license"
)

const (
	// postgresEnv names the server the PostgreSQL scenarios create their own database on.
	postgresEnv = "SWITCHTENDER_TEST_POSTGRES_DSN"
	// fixturePath is the AWX export every scenario imports.
	fixturePath = "testdata/awx-export.json"
	// projectDir holds the playbooks the imported project's repository is built from.
	projectDir = "testdata/project"
	// waitLimit bounds every wait on a server, a run, or a file, so a broken guarantee fails the
	// scenario rather than hanging it.
	waitLimit = 90 * time.Second
	// hostConfigKey is the provisioning callback key the fixture's template carries, as AWX exports
	// it in the clear. It is a secret: it must not reach any record.
	hostConfigKey = "hck-scenario-0f3c9a7e51d24b6c8e0f"
	// notifyURL is the notification template's address. A webhook address is a secret, sealed at
	// rest, so it must not reach any record either.
	notifyURL = "https://hooks.example.invalid/services/T0/B1/notify-secret-6d1e2f"
	// password is the password every account the suite creates signs in with.
	password = "scenario-password-not-a-secret"
)

// standDownOrFail skips on a machine allowed to lack something, and fails where the suite was
// demanded whole, the same switch the integration suite and the store contracts read.
func standDownOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireFullEnv) == "1" {
		t.Fatalf(requireFullEnv+" is set and "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireTools stands the scenario down, or fails it under the full suite, when a tool it runs
// for real is missing.
func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ansible-playbook", "ansible-inventory"} {
		if _, err := exec.LookPath(bin); err != nil {
			standDownOrFail(t, "%s is not on PATH", bin)
		}
	}
	loomsealBinary(t)
}

// storeKind names the database an install runs on.
type storeKind string

const (
	// onSQLite is a SQLite file beside the install.
	onSQLite storeKind = "sqlite"
	// onPostgres is a database of the install's own on the server postgresEnv names.
	onPostgres storeKind = "postgres"
)

// install is one imported SwitchTender install: its database, its environment, the accounts the
// scenario acts as, and the servers running on it.
type install struct {
	// t is the scenario the install belongs to.
	t *testing.T
	// root holds everything the install writes.
	root string
	// db is the --db value every process is given.
	db string
	// env is the environment every child process runs with.
	env []string
	// markers is where the fixture's playbooks write a file per host they reach.
	markers string
	// runFiles is the directory run credential and fact cache directories are created under.
	runFiles string
	// policyPath is the policy file the servers read, empty when they read none.
	policyPath string
	// tokens are the API tokens by actor: admin, approver, operator, and agent.
	tokens map[string]string
	// servers are every server started on the install, running or not.
	servers []*server
	// secrets are values no record, log, or response may hold.
	secrets []string
	// ids maps object names to the ids the import gave them, by kind.
	ids map[string]map[string]string
	// mu guards secrets, which a scenario's helpers append to.
	mu sync.Mutex
}

// installOptions shapes an install.
type installOptions struct {
	// Store is the database the install runs on.
	Store storeKind
	// Policy is the policy file's text, empty for an install with no policy file.
	Policy string
	// Rego are files written beside the policy file, by path relative to it.
	Rego map[string]string
	// ImportArgs are extra arguments the import runs with, such as AWX's job template list.
	ImportArgs []string
}

// newInstall imports the fixture into a new install and creates the accounts the scenarios act
// as. No server is started; the scenario starts the ones it needs.
func newInstall(t *testing.T, opts installOptions) *install {
	t.Helper()
	requireTools(t)
	root := shortTempDir(t)
	in := &install{
		t:        t,
		root:     root,
		markers:  filepath.Join(root, "marks"),
		runFiles: filepath.Join(root, "tmp", runFilesDirName()),
		tokens:   map[string]string{},
		ids:      map[string]map[string]string{},
		secrets:  []string{hostConfigKey, notifyURL},
	}
	for _, dir := range []string{"marks", "home", "tmp", "identity", "logs", "db"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	switch opts.Store {
	case onPostgres:
		in.db = ownDatabase(t)
	default:
		in.db = filepath.Join(root, "db", "switchtender.db")
	}
	in.env = in.childEnv()
	if opts.Policy != "" {
		in.writePolicy(opts.Policy, opts.Rego)
	}

	repo := buildRepository(t, filepath.Join(root, "repo"))
	export := renderFixture(t, root, repo, in.markers)
	out := in.cli(append([]string{"import", "awx", export, "--db", in.db, "--apply"},
		opts.ImportArgs...)...)
	if !strings.Contains(out, "creat") {
		t.Fatalf("the import did not report what it created:\n%s", out)
	}
	in.cli("user", "new", "approver", "--role", "admin", "--db", in.db)
	in.cli("user", "new", "operator", "--role", "operator", "--db", in.db)
	in.tokens["admin"] = tokenFrom(t, in.cli("token", "new", "--name", "setup", "--db", in.db))
	in.tokens["approver"] = tokenFrom(t, in.cli("token", "new", "--name", "approver-laptop",
		"--user", "approver", "--db", in.db))
	in.tokens["operator"] = tokenFrom(t, in.cli("token", "new", "--name", "operator-laptop",
		"--user", "operator", "--db", in.db))
	in.tokens["agent"] = tokenFrom(t, in.cli("token", "new", "--name", "release-agent",
		"--user", "operator", "--agent", "--db", in.db))
	for _, tok := range in.tokens {
		in.addSecret(tok)
	}
	return in
}

// shortTempDir returns a test temporary directory with a short path. Ansible and the run directory
// sweep both put paths under it, and macOS temporary paths are long enough to crowd a socket name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNNER_TEMP")
	if base == "" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "stmig-")
	if err != nil {
		t.Fatalf("create the install directory: %v", err)
	}
	if os.Getenv(keepEnv) != "" {
		t.Logf("keeping the install directory %s", dir)
		return dir
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// keepEnv leaves each install's directory in place after the scenario, for reading a failure.
const keepEnv = "SWITCHTENDER_SCENARIO_KEEP"

// runFilesDirName is the name the dispatcher gives its run directory root under the temporary
// directory, per account.
func runFilesDirName() string {
	return fmt.Sprintf("switchtender-runfiles-%d", os.Getuid())
}

// addSecret records a value no record may hold.
func (in *install) addSecret(v string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.secrets = append(in.secrets, v)
}

// childEnv is the environment every child process runs with: the parent's, less anything that
// would reach another install, plus this install's keys, license, and directories.
func (in *install) childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "SWITCHTENDER_"), strings.HasPrefix(name, "ANSIBLE_"),
			name == "HOME", name == "TMPDIR", name == "XDG_CACHE_HOME", name == "XDG_CONFIG_HOME":
			continue
		// A runtime directory would take precedence over the temporary directory for run files,
		// and every install would then share the runner's, where the scenarios do not look.
		case name == "XDG_RUNTIME_DIR", name == "RUNTIME_DIRECTORY":
			continue
		}
		env = append(env, kv)
	}
	key, salt, auditKey := randomHex(in.t, 32), randomHex(in.t, 16), randomHex(in.t, 32)
	in.secrets = append(in.secrets, key, salt, auditKey)
	return append(env,
		childEnv+"=1",
		licenseKeyEnv+"="+hex.EncodeToString(licensePublicKey(in.t)),
		"SWITCHTENDER_LICENSE="+writeLicense(in.t, in.root),
		"SWITCHTENDER_ENCRYPTION_KEY="+key,
		"SWITCHTENDER_ENCRYPTION_SALT="+salt,
		"SWITCHTENDER_IDENTITY_DIR="+filepath.Join(in.root, "identity"),
		// Every process on the install signs as one producer, which a shared PostgreSQL chain
		// requires and which the documented multi-replica deployment sets.
		"SWITCHTENDER_AUDIT_KEY="+auditKey,
		"SWITCHTENDER_PASSWORD="+password,
		"HOME="+filepath.Join(in.root, "home"),
		"TMPDIR="+filepath.Join(in.root, "tmp"),
		"XDG_CACHE_HOME="+filepath.Join(in.root, "home", ".cache"),
		"XDG_CONFIG_HOME="+filepath.Join(in.root, "home", ".config"),
		"ANSIBLE_LOCAL_TEMP="+filepath.Join(in.root, "home", ".ansible", "tmp"),
		"ANSIBLE_NOCOLOR=1",
	)
}

// cli runs one switchtender command to completion and returns its combined output, failing the
// scenario when it fails.
func (in *install) cli(args ...string) string {
	in.t.Helper()
	out, err := in.cliResult(args...)
	if err != nil {
		in.t.Fatalf("switchtender %s: %v\n%s", strings.Join(redactArgs(args), " "), err, out)
	}
	return out
}

// cliResult runs one switchtender command to completion and returns its combined output and error.
func (in *install) cliResult(args ...string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c := exec.CommandContext(ctx, self, args...)
	c.Env = in.env
	c.Dir = in.root
	out, err := c.CombinedOutput()
	return string(out), err
}

// redactArgs hides a DSN's password in a command line printed on failure.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if u, err := url.Parse(a); err == nil && u.User != nil {
			u.User = url.User(u.User.Username())
			a = u.String()
		}
		out[i] = a
	}
	return out
}

// tokenFrom reads the token a token new command printed.
func tokenFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		var printed map[string]string
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &printed) == nil && printed["token"] != "" {
			return printed["token"]
		}
	}
	t.Fatalf("no token in the command's output:\n%s", out)
	return ""
}

// writePolicy writes the policy file the servers read, and the Rego modules beside it. A running
// server picks up the change without a restart.
func (in *install) writePolicy(text string, rego map[string]string) {
	in.t.Helper()
	dir := filepath.Join(in.root, "policy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		in.t.Fatalf("create the policy directory: %v", err)
	}
	for rel, body := range rego {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			in.t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			in.t.Fatalf("write %s: %v", rel, err)
		}
	}
	in.policyPath = filepath.Join(dir, "policies.yaml")
	if err := os.WriteFile(in.policyPath, []byte(text), 0o600); err != nil {
		in.t.Fatalf("write the policy file: %v", err)
	}
}

// randomHex returns n random bytes in hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(b)
}

// licenseKeys is the keypair the suite's license is signed with, minted once per test process.
var licenseKeys = sync.OnceValues(func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("scenario: generate the license key: " + err.Error())
	}
	return pub, priv
})

// licensePublicKey returns the public half of the suite's license key.
func licensePublicKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _ := licenseKeys()
	return pub
}

// writeLicense writes a Team license signed by the suite's key into dir and returns its path.
func writeLicense(t *testing.T, dir string) string {
	t.Helper()
	_, priv := licenseKeys()
	raw, err := license.Sign(license.Claims{
		V: 1, ID: "lic_scenario", Org: "KordLoom scenarios", Tier: license.TierTeam,
		Issued:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Expires: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), Kid: licenseKid,
	}, priv)
	if err != nil {
		t.Fatalf("sign the scenario license: %v", err)
	}
	path := filepath.Join(dir, "switchtender-license.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write the scenario license: %v", err)
	}
	return path
}

// buildRepository creates the git repository the imported project clones, holding the fixture's
// playbooks on main, and returns its file URL. It is built with the git library rather than the git
// command, so the suite neither needs git configured nor touches any repository but its own.
func buildRepository(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("init the project repository: %v", err)
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("read the project playbooks: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open the project worktree: %v", err)
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(projectDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
		if _, err := wt.Add(e.Name()); err != nil {
			t.Fatalf("stage %s: %v", e.Name(), err)
		}
	}
	if _, err := wt.Commit("playbooks", &git.CommitOptions{
		Author: &object.Signature{Name: "scenario", Email: "scenario@example.invalid", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit the playbooks: %v", err)
	}
	return "file://" + dir
}

// renderFixture writes the AWX export with this install's repository, marker directory, and secrets
// filled in, and returns its path.
func renderFixture(t *testing.T, root, repoURL, markers string) string {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	text := strings.NewReplacer(
		"__REPO_URL__", repoURL,
		"__MARKER_DIR__", markers,
		"__NOTIFY_URL__", notifyURL,
		"__HOST_CONFIG_KEY__", hostConfigKey,
	).Replace(string(raw))
	path := filepath.Join(root, "awx-export.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write the rendered fixture: %v", err)
	}
	return path
}

// ownDatabase creates a database of this scenario's own on the server postgresEnv names and returns
// a DSN pointing at it, dropping it when the scenario ends. A deployed install owns its database,
// and two scenarios sharing one would race each other's chains.
func ownDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresEnv)
	if dsn == "" {
		standDownOrFail(t, "%s is not set, so the PostgreSQL scenario cannot run", postgresEnv)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the server %s names: %v", postgresEnv, err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("st_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create the scenario database: %v", err)
	}
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresEnv, err)
	}
	u.Path = "/" + name
	return u.String()
}

// server is one switchtender serve process.
type server struct {
	// name labels the server in failures and names its log file.
	name string
	// url is the base URL it listens on.
	url string
	// cmd is the running process.
	cmd *exec.Cmd
	// logPath is the file its output goes to.
	logPath string
	// done is closed when the process exits.
	done chan struct{}
	// exitErr is the process's exit error, valid once done is closed.
	exitErr error
}

// startServer starts a server on the install with the scenario's extra flags and waits until it is
// ready to serve.
func (in *install) startServer(name string, extra ...string) *server {
	in.t.Helper()
	port := freePort(in.t)
	args := []string{"serve", "--db", in.db, "--addr", "127.0.0.1:" + port,
		"--forward-state", filepath.Join(in.root, "forward-"+name+".json")}
	if in.policyPath != "" {
		args = append(args, "--policy-file", in.policyPath)
	}
	args = append(args, extra...)
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	logPath := filepath.Join(in.root, "logs", fmt.Sprintf("%s-%d.log", name, len(in.servers)))
	logFile, err := os.Create(logPath)
	if err != nil {
		in.t.Fatalf("create the server log: %v", err)
	}
	c := exec.Command(self, args...)
	c.Env = in.env
	c.Dir = in.root
	c.Stdout, c.Stderr = logFile, logFile
	if err := c.Start(); err != nil {
		in.t.Fatalf("start server %s: %v", name, err)
	}
	s := &server{name: name, url: "http://127.0.0.1:" + port, cmd: c, logPath: logPath,
		done: make(chan struct{})}
	go func() {
		s.exitErr = c.Wait()
		_ = logFile.Close()
		close(s.done)
	}()
	in.servers = append(in.servers, s)
	in.t.Cleanup(func() { s.stop() })

	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			log, _ := os.ReadFile(logPath)
			in.t.Fatalf("server %s exited before it was ready: %v\n%s", name, s.exitErr, log)
		default:
		}
		res, err := http.Get(s.url + "/readyz")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(logPath)
	in.t.Fatalf("server %s was not ready within %s:\n%s", name, waitLimit, log)
	return nil
}

// stop ends the server gracefully and waits for it, killing it if it does not stop.
func (s *server) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// kill ends the server the way a crash or an OOM kill does, with no chance to clean up.
func (s *server) kill() {
	_ = s.cmd.Process.Kill()
	<-s.done
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// response is one API answer.
type response struct {
	// Status is the HTTP status.
	Status int
	// Body is the raw body.
	Body []byte
	// Header is the response headers.
	Header http.Header
}

// decode decodes the body into v, failing the scenario when it does not decode.
func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

// httpClient is the client every API call shares, with a deadline so a stalled server fails the
// scenario rather than hanging it.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// api calls the server as actor, which names a token, and returns the answer. Every answer is kept
// for the leak scan, since an API response is one of the places a secret must never appear.
func (in *install) api(s *server, actor, method, path string, body any) response {
	in.t.Helper()
	return in.apiHeaders(s, actor, method, path, body, nil)
}

// apiHeaders is api with extra request headers.
func (in *install) apiHeaders(s *server, actor, method, path string, body any,
	headers map[string]string) response {
	in.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			in.t.Fatalf("encode the request: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		in.t.Fatalf("build the request: %v", err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		tok, ok := in.tokens[actor]
		if !ok {
			in.t.Fatalf("no token for actor %q", actor)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		in.t.Fatalf("read %s %s: %v", method, path, err)
	}
	in.recordAnswer(method+" "+path, raw)
	return response{Status: res.StatusCode, Body: raw, Header: res.Header}
}

// must calls the API and fails the scenario unless the status is one of want.
func (in *install) must(s *server, actor, method, path string, body any, want ...int) response {
	in.t.Helper()
	r := in.api(s, actor, method, path, body)
	for _, w := range want {
		if r.Status == w {
			return r
		}
	}
	in.t.Fatalf("%s %s as %s = %d, want %v: %s", method, path, actor, r.Status, want, r.Body)
	return r
}

// answers are every API body this test process received, by install root, for the leak scan.
var answers sync.Map

// recordAnswer keeps one API body for the leak scan.
func (in *install) recordAnswer(what string, body []byte) {
	v, _ := answers.LoadOrStore(in.root, &answerLog{})
	log := v.(*answerLog)
	log.mu.Lock()
	defer log.mu.Unlock()
	log.bodies = append(log.bodies, answer{What: what, Body: append([]byte(nil), body...)})
}

// answerLog is one install's received API bodies.
type answerLog struct {
	// mu guards bodies.
	mu sync.Mutex
	// bodies are the bodies in the order they arrived.
	bodies []answer
}

// answer is one API body and what it answered.
type answer struct {
	// What is the method and path.
	What string
	// Body is the body.
	Body []byte
}
