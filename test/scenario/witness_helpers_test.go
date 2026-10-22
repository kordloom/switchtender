package scenario

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kordloom/switchtender/cmd"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/witness"
)

const (
	// witnessChildEnv marks a process this package started as the switchtender command rather than
	// as the test binary.
	witnessChildEnv = "SWITCHTENDER_WITNESS_SCENARIO_CHILD"
	// witnessLicenseKeyEnv carries the hex public key the child trusts for the license the
	// witness scenarios mint.
	witnessLicenseKeyEnv = "SWITCHTENDER_WITNESS_SCENARIO_LICENSE_KEY"
	// witnessLicenseKid is the key id the scenario license names.
	witnessLicenseKid = "witness-scenario"
	// witnessWait bounds every wait on the real span cadence, so a stalled server fails the test
	// rather than hanging it.
	witnessWait = 30 * time.Second
	// witnessCadence is the --span-cadence every scenario server runs with.
	witnessCadence = "1s"
)

// TestMain runs the package's tests, or, in a child a witness scenario started, the switchtender
// command.
func TestMain(m *testing.M) {
	if os.Getenv(witnessChildEnv) == "1" {
		runWitnessChild()
		return
	}
	os.Exit(m.Run())
}

// runWitnessChild trusts the scenario license key and runs the switchtender command with this
// process's arguments. It never returns: the command exits the process with its own code.
func runWitnessChild() {
	pub, err := hex.DecodeString(os.Getenv(witnessLicenseKeyEnv))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		fmt.Fprintln(os.Stderr, "witness scenario child: the license key is not an ed25519 key")
		os.Exit(2)
	}
	license.RegisterKey(witnessLicenseKid, ed25519.PublicKey(pub))
	os.Args = append([]string{"switchtender"}, os.Args[1:]...)
	cmd.Execute(nil)
}

// witnessLicenseKeys is the key pair the scenario licenses are signed with, made once per process.
var witnessLicenseKeys = sync.OnceValues(func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("witness scenario: generate the license key: " + err.Error())
	}
	return pub, priv
})

// scenarioClock is the witness's clock: real time, until a test freezes it so two statements the
// witness signs with no push between them are byte-identical.
type scenarioClock struct {
	// frozen is the instant the clock reads while frozen, nil while it follows real time.
	frozen atomic.Pointer[time.Time]
}

// now reads the clock.
func (c *scenarioClock) now() time.Time {
	if at := c.frozen.Load(); at != nil {
		return *at
	}
	return time.Now()
}

// freeze stops the clock at the current instant.
func (c *scenarioClock) freeze() {
	at := time.Now()
	c.frozen.Store(&at)
}

// thaw returns the clock to real time, which is never behind the frozen instant.
func (c *scenarioClock) thaw() { c.frozen.Store(nil) }

// scenarioWitness is a real push witness in open policy behind a front the test controls, which
// can answer the statement fetch or the pushes 502 the way a tunnel in front of a stopped service
// does, while everything else reaches the service.
type scenarioWitness struct {
	// url is the front's base URL, the one installs enroll with.
	url string
	// keyID is the witness key id a relying party pins.
	keyID string
	// inner is the service's own handler, which the test reads statements and findings from
	// without passing the front.
	inner http.Handler
	// clock is the service's clock.
	clock *scenarioClock
	// statementDown makes the front answer every statement fetch 502.
	statementDown atomic.Bool
	// pushesDown makes the front answer every push 502.
	pushesDown atomic.Bool
	// statementsBlocked counts the statement fetches the front answered 502.
	statementsBlocked atomic.Int64
	// pushesBlocked counts the pushes the front answered 502.
	pushesBlocked atomic.Int64
}

// newScenarioWitness starts a push witness in open policy and its front.
func newScenarioWitness(t *testing.T) *scenarioWitness {
	t.Helper()
	id, err := audit.LoadWitnessIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	clock := &scenarioClock{}
	svc, err := witness.NewService(id, t.TempDir(), time.Minute, nil, nil, nil,
		witness.WithAcceptEnrollments(witness.DefaultMaxInstalls),
		witness.WithEnrollmentPolicy(witness.OpenEnrollment),
		witness.WithServiceClock(clock.now),
		witness.WithWitnessKeys([]witness.WitnessKey{
			witness.WitnessKeyOf(id, time.Now().Add(-time.Hour))}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w := &scenarioWitness{keyID: id.KeyID(), inner: svc.Handler(), clock: clock}
	front := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/attestation") &&
			w.statementDown.Load():
			w.statementsBlocked.Add(1)
			http.Error(rw, `{"error":"bad gateway"}`, http.StatusBadGateway)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/beats") &&
			w.pushesDown.Load():
			w.pushesBlocked.Add(1)
			http.Error(rw, `{"error":"bad gateway"}`, http.StatusBadGateway)
			return
		}
		w.inner.ServeHTTP(rw, r)
	}))
	t.Cleanup(front.Close)
	w.url = front.URL
	return w
}

// read answers one GET from the service itself, past the front, and fails the test unless it
// answers 200.
func (w *scenarioWitness) read(t *testing.T, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	w.inner.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// statement returns the statement the witness signs for the producer key now, raw and decoded.
func (w *scenarioWitness) statement(t *testing.T, keyID string) ([]byte, *witness.Statement) {
	t.Helper()
	raw := w.read(t, "/v1/installs/"+keyID+"/attestation")
	var st witness.Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode the statement %s: %v", raw, err)
	}
	return raw, &st
}

// installFindings is the witness's findings record for one install.
type installFindings struct {
	// Findings is the tail of the record, oldest first.
	Findings []witness.RecordedFinding `json:"findings"`
	// RefusalsTotal counts every refusal recorded.
	RefusalsTotal int64 `json:"refusals_total"`
	// NotesTotal counts every note recorded.
	NotesTotal int64 `json:"notes_total"`
}

// findings returns the witness's findings record for the producer key.
func (w *scenarioWitness) findings(t *testing.T, keyID string) installFindings {
	t.Helper()
	raw := w.read(t, "/v1/installs/"+keyID+"/findings")
	var f installFindings
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode the findings %s: %v", raw, err)
	}
	return f
}

// witnessInstall is one SQLite install enrolled with a scenario witness, driven through the
// switchtender command in child processes.
type witnessInstall struct {
	// t is the test the install belongs to.
	t *testing.T
	// dir holds the database, the license, the enrollment, and the producer key.
	dir string
	// db is the SQLite file.
	db string
	// env is the child's environment.
	env []string
	// token is an administrator token for the status route.
	token string
	// keyID is the producer key id, the fingerprint a relying party pins.
	keyID string
	// installID is the install the producer key signs for.
	installID string
	// servers counts the servers started, to name their logs.
	servers int
}

// newWitnessInstall creates an install with switchtender init, a Team license carrying the hosted
// witness add-on, an administrator token, and an enrollment with w made by witness enroll.
func newWitnessInstall(t *testing.T, w *scenarioWitness) *witnessInstall {
	t.Helper()
	dir := t.TempDir()
	in := &witnessInstall{t: t, dir: dir, db: filepath.Join(dir, "switchtender.db")}
	pub, priv := witnessLicenseKeys()
	raw, err := license.Sign(license.Claims{
		V: 1, ID: "lic_witness_scenario", Org: "witness scenarios", Tier: license.TierTeam,
		Issued:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Expires: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		Kid:     witnessLicenseKid, Addons: []string{license.AddonHostedWitness},
	}, priv)
	if err != nil {
		t.Fatalf("sign the scenario license: %v", err)
	}
	if err := os.WriteFile(license.PathFor(in.db), raw, 0o600); err != nil {
		t.Fatalf("write the scenario license: %v", err)
	}
	for _, sub := range []string{"home", "tmp", "logs"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SWITCHTENDER_") || name == "HOME" || name == "TMPDIR" {
			continue
		}
		in.env = append(in.env, kv)
	}
	in.env = append(in.env, witnessChildEnv+"=1",
		witnessLicenseKeyEnv+"="+hex.EncodeToString(pub),
		"HOME="+filepath.Join(dir, "home"), "TMPDIR="+filepath.Join(dir, "tmp"))

	config := filepath.Join(dir, "switchtender.env")
	out, err := in.cli("init", "--db", in.db, "--config", config)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	written, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read the config init wrote: %v", err)
	}
	for line := range strings.SplitSeq(string(written), "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok &&
			strings.HasPrefix(name, "SWITCHTENDER_") {
			in.env = append(in.env, name+"="+strings.Trim(value, `'"`))
		}
	}
	out, err = in.cli("token", "new", "--name", "witness-scenario", "--db", in.db)
	if err != nil {
		t.Fatalf("token new: %v\n%s", err, out)
	}
	in.token = printedMember(t, out, "token")
	out, err = in.cli("witness", "enroll", "--db", in.db, "--witness", w.url,
		"--witness-key", w.keyID)
	if err != nil {
		t.Fatalf("witness enroll: %v\n%s", err, out)
	}
	in.keyID = printedMember(t, out, "key_id")
	in.installID = printedMember(t, out, "install_id")
	if _, err := os.Stat(witnessclient.PathFor(license.PathFor(in.db))); err != nil {
		t.Fatalf("witness enroll wrote no enrollment beside the license: %v\n%s", err, out)
	}
	return in
}

// printedMember returns the string member name of the first JSON object line in out.
func printedMember(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		var printed map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &printed) != nil {
			continue
		}
		if v, ok := printed[name].(string); ok && v != "" {
			return v
		}
	}
	t.Fatalf("no %s in the command's output:\n%s", name, out)
	return ""
}

// cli runs one switchtender command to completion and returns its combined output.
func (in *witnessInstall) cli(args ...string) (string, error) {
	in.t.Helper()
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), witnessWait)
	defer cancel()
	c := exec.CommandContext(ctx, self, args...)
	c.Env, c.Dir = in.env, in.dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// serveProcess is one switchtender serve child.
type serveProcess struct {
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

// launch starts serve on the install with --span-cadence and returns the process without waiting
// for it.
func (in *witnessInstall) launch() *serveProcess {
	in.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		in.t.Fatalf("find a free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	in.servers++
	logPath := filepath.Join(in.dir, "logs", fmt.Sprintf("serve-%d.log", in.servers))
	logFile, err := os.Create(logPath)
	if err != nil {
		in.t.Fatalf("create the server log: %v", err)
	}
	c := exec.Command(self, "serve", "--db", in.db, "--addr", addr,
		"--span-cadence", witnessCadence)
	c.Env, c.Dir = in.env, in.dir
	c.Stdout, c.Stderr = logFile, logFile
	if err := c.Start(); err != nil {
		in.t.Fatalf("start serve: %v", err)
	}
	s := &serveProcess{url: "http://" + addr, cmd: c, logPath: logPath, done: make(chan struct{})}
	go func() {
		s.exitErr = c.Wait()
		_ = logFile.Close()
		close(s.done)
	}()
	in.t.Cleanup(s.stop)
	return s
}

// start starts serve and waits until it answers /readyz, failing the test when it exits first.
func (in *witnessInstall) start() *serveProcess {
	in.t.Helper()
	s := in.launch()
	deadline := time.Now().Add(witnessWait)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			in.t.Fatalf("serve exited before it was ready: %v\n%s", s.exitErr, s.log())
		default:
		}
		if res, err := http.Get(s.url + "/readyz"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	in.t.Fatalf("serve was not ready within %s:\n%s", witnessWait, s.log())
	return nil
}

// startRefused starts serve and waits for it to exit, failing the test when it becomes ready or
// exits cleanly. It returns what the server printed.
func (in *witnessInstall) startRefused() string {
	in.t.Helper()
	s := in.launch()
	select {
	case <-s.done:
	case <-time.After(witnessWait):
		in.t.Fatalf("serve over a rewritten chain kept running for %s, want the start refused:\n%s",
			witnessWait, s.log())
	}
	var exit *exec.ExitError
	if !errors.As(s.exitErr, &exit) || exit.ExitCode() == 0 {
		in.t.Fatalf("serve over a rewritten chain exited with %v, want a refused start:\n%s",
			s.exitErr, s.log())
	}
	return s.log()
}

// log returns what the server has printed so far.
func (s *serveProcess) log() string {
	raw, _ := os.ReadFile(s.logPath)
	return string(raw)
}

// stop ends the server gracefully and waits for it, killing it if it does not stop.
func (s *serveProcess) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(witnessWait):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// status reads the server's witness status route as the administrator.
func (in *witnessInstall) status(s *serveProcess) witnessclient.Status {
	in.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+"/v1/audit/witness", nil)
	if err != nil {
		in.t.Fatalf("build the status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+in.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		in.t.Fatalf("GET /v1/audit/witness: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var st witnessclient.Status
	if res.StatusCode != http.StatusOK {
		in.t.Fatalf("GET /v1/audit/witness = %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		in.t.Fatalf("decode the witness status: %v", err)
	}
	return st
}

// waitFor polls cond until it holds, failing the test with what after witnessWait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(witnessWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", witnessWait, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// export writes the install's audit chain as a signed bundle with audit bundle and returns its
// path. The server must be stopped, so the export reads a chain nothing is appending to.
func (in *witnessInstall) export(name string) string {
	in.t.Helper()
	path := filepath.Join(in.dir, name)
	if out, err := in.cli("audit", "bundle", "--db", in.db, "--out", path); err != nil {
		in.t.Fatalf("audit bundle: %v\n%s", err, out)
	}
	return path
}

// keep writes a statement the witness signed to a file verify can read and returns its path.
func (in *witnessInstall) keep(name string, raw []byte) string {
	in.t.Helper()
	path := filepath.Join(in.dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		in.t.Fatalf("write the statement: %v", err)
	}
	return path
}

// verify runs verify on bundle pinned to the producer key and the witness key and held to the
// statement file, and returns what it printed and whether it exited zero.
func (in *witnessInstall) verify(w *scenarioWitness, bundle, statement string) (string, bool) {
	in.t.Helper()
	out, err := in.cli("verify", bundle, "--pubkey", in.keyID, "--witness-pubkey", w.keyID,
		"--witness", statement)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		in.t.Fatalf("run verify: %v\n%s", err, out)
	}
	return out, err == nil
}

// chain reads every audit entry from the database, oldest first. The server must be stopped.
func (in *witnessInstall) chain() []*audit.Entry {
	in.t.Helper()
	raw, err := sql.Open("sqlite", in.db)
	if err != nil {
		in.t.Fatalf("open the database: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query("SELECT id, at, actor, actor_type, on_behalf_of, method, path, " +
		"content_digest, seq, prev_hash, hash, install_id FROM audit_entries ORDER BY seq ASC")
	if err != nil {
		in.t.Fatalf("read the chain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var entries []*audit.Entry
	for rows.Next() {
		var e audit.Entry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.PrevHash, &e.Hash, &e.InstallID); err != nil {
			in.t.Fatalf("scan an entry: %v", err)
		}
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			in.t.Fatalf("parse an entry time: %v", err)
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		in.t.Fatalf("read the chain: %v", err)
	}
	return entries
}

// rewrite is the database writer: it changes the path of the entry at seq and re-links every
// entry after it, with nothing but SQL, so the chain verifies on its own and every link and tree
// root from seq on differs from what the witness countersigned. It returns the rewritten entry's
// old path.
func (in *witnessInstall) rewrite(seq int64) string {
	in.t.Helper()
	entries := in.chain()
	raw, err := sql.Open("sqlite", in.db)
	if err != nil {
		in.t.Fatalf("open the database: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var prev *audit.Entry
	var was string
	for _, e := range entries {
		switch {
		case e.Seq < seq:
			prev = e
			continue
		case e.Seq == seq:
			was = e.Path
			e.Path = "/v1/rewritten"
		}
		if prev != nil {
			e.PrevHash = prev.Hash
		}
		e.Hash = audit.EntryHash(e)
		if _, err := raw.Exec("UPDATE audit_entries SET path=?, prev_hash=?, hash=? WHERE id=?",
			e.Path, e.PrevHash, e.Hash, e.ID); err != nil {
			in.t.Fatalf("rewrite entry %d: %v", e.Seq, err)
		}
		prev = e
	}
	if was == "" {
		in.t.Fatalf("the chain holds no entry at seq %d", seq)
	}
	return was
}
