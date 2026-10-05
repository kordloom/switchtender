package relay_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// Secrets the end-to-end delivery test hands a relay worker, looked for everywhere they must not
// be.
const (
	// e2ePassphrase unlocks the SSH key credential.
	e2ePassphrase = "e2e-key-passphrase-2b7e"
	// e2eAnswer is the secret survey answer.
	e2eAnswer = "e2e-survey-answer-f19c"
	// e2eEnvValue is the env credential's secret value.
	e2eEnvValue = "e2e-env-secret-6a40"
	// e2eKubeToken is the token in the typed credential's file.
	e2eKubeToken = "e2e-kube-token-93d5"
)

// wireRecorder keeps every request and response the relay carried, headers and bodies, the way a
// proxy or load balancer logging relay traffic would.
type wireRecorder struct {
	// mu guards log.
	mu sync.Mutex
	// log is everything recorded.
	log bytes.Buffer
}

// wrap records each exchange through next.
func (w *wireRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		w.mu.Lock()
		fmt.Fprintf(&w.log, "%s %s %v\n%s\n%d %v\n%s\n", r.Method, r.URL, r.Header, body, rec.Code,
			rec.Header(), rec.Body.Bytes())
		w.mu.Unlock()
		for k, v := range rec.Header() {
			rw.Header()[k] = v
		}
		rw.WriteHeader(rec.Code)
		_, _ = rw.Write(rec.Body.Bytes())
	})
}

// text returns everything recorded.
func (w *wireRecorder) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.log.String()
}

// toolView is what the relay worker's tool was handed.
type toolView struct {
	// Key is the private key file's content.
	Key string
	// KeyMode is the private key file's mode.
	KeyMode os.FileMode
	// Answer is the survey answer variable.
	Answer string
	// Env is the env credential's variable.
	Env string
	// Kube is the typed credential's file content.
	Kube string
	// Paths are every credential path the tool was handed.
	Paths []string
}

// viewingRunner records what a tool is handed and prints the secrets the way a careless play would,
// so masking is exercised on the worker.
type viewingRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen is what the one run was handed.
	seen *toolView
}

// Run records the run's secrets and succeeds.
func (v *viewingRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	view := &toolView{Answer: fmt.Sprint(spec.ExtraVars["db_password"])}
	if raw, err := os.ReadFile(spec.PrivateKeyPath); err == nil {
		view.Key = string(raw)
	}
	if info, err := os.Stat(spec.PrivateKeyPath); err == nil {
		view.KeyMode = info.Mode().Perm()
	}
	for _, e := range spec.Env {
		k, val, _ := strings.Cut(e, "=")
		switch k {
		case "CLOUD_TOKEN":
			view.Env = val
		case "KUBECONFIG":
			if raw, err := os.ReadFile(val); err == nil {
				view.Kube = string(raw)
			}
		}
	}
	view.Paths = append(append([]string{spec.PrivateKeyPath}, spec.ExtraVarsFiles...),
		spec.CredentialFiles...)
	v.mu.Lock()
	v.seen = view
	v.mu.Unlock()
	_, _ = fmt.Fprintf(out, "answer %s env %s\nkube %s\nkey %s\n", view.Answer, view.Env, view.Kube,
		view.Key)
	return roundhouse.Result{ExitCode: 0}, nil
}

// view returns what the run was handed.
func (v *viewingRunner) view() *toolView {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seen
}

// TestARelayRunReceivesItsSecretsAndLeavesThemNowhere is the end-to-end delivery. A control node
// with a SQLite store and a relay worker with no store and no encryption key are joined only by the
// HTTP relay. The worker executes a run that needs an SSH key unlocked with its passphrase, an env
// credential, a custom type's file, and a secret survey answer. The tool receives every value, and
// afterward none of them is in either process's log, in the audit chain, in the database, in the
// run's record, or anywhere in the relay traffic, which carries only ciphertext. The chain records
// which pool and worker received which credentials, and still verifies.
//
//nolint:funlen // Test function.
func TestARelayRunReceivesItsSecretsAndLeavesThemNowhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sealer := credential.NewSealer("e2e-control-node-key", "e2e-control-node-salt")
	seal := func(plain string) string {
		t.Helper()
		sealed, err := sealer.Seal(plain)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		return sealed
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(edKey, "", []byte(e2ePassphrase))
	if err != nil {
		t.Fatalf("MarshalPrivateKeyWithPassphrase() error = %v", err)
	}
	lockedKey := string(pem.EncodeToMemory(block))
	typ := &credential.CredentialType{ID: "ctype_kube", Name: "Kubeconfig",
		Fields:        []credential.Field{{Name: "kubeconfig", Secret: true, Multiline: true}},
		FileInjectors: map[string]string{"template": "{{ kubeconfig }}"},
		EnvInjectors:  map[string]string{"KUBECONFIG": "{{ tower.filename }}"}}
	if err := db.CredentialTypes().Save(ctx, typ); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	kubeFields, err := json.Marshal(map[string]string{
		"kubeconfig": "apiVersion: v1\nusers:\n- user:\n    token: " + e2eKubeToken + "\n"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, c := range []*credential.Credential{
		{ID: "cred_ssh", Name: "fleet key", Kind: credential.KindSSHKey,
			Secret: seal(credential.BuildSSHKeySecret(lockedKey, e2ePassphrase))},
		{ID: "cred_env", Name: "cloud", Kind: credential.KindEnv,
			Secret: seal("CLOUD_TOKEN=" + e2eEnvValue)},
		{ID: "cred_kube", Name: "kube", TypeID: "ctype_kube", Secret: seal(string(kubeFields))},
	} {
		if err := db.Credentials().Save(ctx, c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	controlLog, controlLogs := encodedLogger()
	control := dispatch.New(db.Runs(), roundhouse.RunnerFunc(nil), controlLog,
		dispatch.WithNoJanitor(), dispatch.WithCredentials(db.Credentials(), sealer),
		dispatch.WithCredentialTypes(db.CredentialTypes()), dispatch.WithInventories(db.Inventories()),
		dispatch.WithAudits(db.Audits()))
	t.Cleanup(control.Close)

	poolKey, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	keyPath := filepath.Join(dir, "delivery.key")
	if err := handoff.WriteKeyFile(keyPath, poolKey); err != nil {
		t.Fatalf("WriteKeyFile() error = %v", err)
	}
	pools, err := relay.LoadPools(writePoolFile(t, "workers:\n  - name: dmz\n    token_sha256: "+
		relay.HashToken("tok-e2e")+"\n    queues: [dmz]\n    delivery_key: "+
		poolKey.Public().String()+"\n"))
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}
	wire := &wireRecorder{}
	ts := httptest.NewServer(wire.wrap(relay.NewHandler(db.Runs(), pools, controlLog, nil,
		db.Audits(), relay.WithSecretOpener(control))))
	t.Cleanup(ts.Close)

	loaded, err := handoff.LoadPrivateKeyFile(keyPath)
	if err != nil {
		t.Fatalf("LoadPrivateKeyFile() error = %v", err)
	}
	ring, err := handoff.NewKeyRing(loaded)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	workerLog, workerLogs := encodedLogger()
	transport := relay.NewHTTPTransport(ts.URL, "tok-e2e", ts.Client())
	runner := &viewingRunner{}
	root := filepath.Join(dir, "worker-runfiles")
	worker := dispatch.New(relay.NewClient(transport), runner, workerLog,
		dispatch.WithOwner("relay-a"), dispatch.WithQueues([]string{"dmz"}), dispatch.WithNoJanitor(),
		dispatch.WithClaimInterval(5*time.Millisecond),
		dispatch.WithPolicies(relay.NewPolicyClient(transport)),
		dispatch.WithSecretDelivery(relay.NewReceiver(transport, ring)),
		dispatch.WithRunFilesRoot(root))
	t.Cleanup(worker.Close)

	if err := db.Runs().Save(ctx, &run.Run{
		ID: "run_e2e", Tool: run.ToolBash, Command: "deploy", Status: run.StatusPending, Queue: "dmz",
		CredentialIDs: []string{"cred_ssh", "cred_env", "cred_kube"},
		SealedNames:   []string{"db_password"},
		SealedVars:    map[string]string{"db_password": seal(e2eAnswer)}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	final := waitRelayTerminal(t, db.Runs(), "run_e2e")
	if final.Status != run.StatusSucceeded || final.ClaimedBy != "relay-a" {
		t.Fatalf("run = %s by %q (%s), want succeeded on the relay worker", final.Status,
			final.ClaimedBy, final.Error)
	}

	// The tool received every secret, in the form it reads.
	view := runner.view()
	if view == nil {
		t.Fatal("the relay worker never ran the tool")
	}
	got, want := keyFingerprint(t, view.Key), keyFingerprint(t, lockedKeyUnlocked(t, lockedKey))
	if got != want {
		t.Errorf("the tool's key is %s, want the credential's key %s", got, want)
	}
	if strings.Contains(view.Key, "ENCRYPTED") || view.KeyMode != 0o600 {
		t.Errorf("the key was handed over still locked, or mode %v, want unlocked and 0600", view.KeyMode)
	}
	if view.Answer != e2eAnswer || view.Env != e2eEnvValue ||
		!strings.Contains(view.Kube, e2eKubeToken) {
		t.Errorf("the tool was handed answer %q, env %q, kube %q", view.Answer, view.Env, view.Kube)
	}
	for _, p := range view.Paths {
		if !strings.HasPrefix(p, root+string(os.PathSeparator)) {
			t.Errorf("credential file %s was written outside the run directory root", p)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("credential file %s survived the run", p)
		}
	}
	// The sweep's own lock file may sit in the root once a periodic sweep has run, and is no run's.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Errorf("read the run directory root: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("the run directory root holds %s after the run", e.Name())
		}
	}

	// The chain says which pool and worker received which credentials, and still verifies.
	chain, err := db.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Fatalf("the chain no longer verifies, broken at %d", at)
	}
	wantDelivered := relay.DeliveryPath("run_e2e", poolKey.ID(),
		[]string{"cred_env", "cred_kube", "cred_ssh"}, []string{"db_password"})
	var found bool
	for _, e := range chain {
		if e.Path == wantDelivered && e.Actor == "pool:dmz worker:relay-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("the chain has no record of the delivery at %s", wantDelivered)
	}

	// No value is anywhere but where the tool read it.
	chainJSON, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	record, err := json.Marshal(final)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	logText, err := db.Runs().Log(ctx, "run_e2e")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	events, err := db.Runs().Events(ctx, "run_e2e")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	eventJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	places := map[string]string{
		"the control node's log": controlLogs.String(), "the worker's log": workerLogs.String(),
		"the relay traffic": wire.text(), "the audit chain": string(chainJSON),
		"the run record": string(record), "the run's log": string(logText),
		"the run's events": string(eventJSON),
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if raw, err := os.ReadFile(filepath.Join(dir, "control.db"+suffix)); err == nil {
			places["the database file control.db"+suffix] = string(raw)
		}
	}
	secrets := []string{e2ePassphrase, e2eAnswer, e2eEnvValue, e2eKubeToken}
	secrets = append(secrets, keyLines(lockedKey)...)
	secrets = append(secrets, keyLines(view.Key)...)
	for where, text := range places {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries a secret value (%d bytes, starting %q)", where, len(secret),
					secret[:6])
			}
		}
	}
	if !strings.Contains(wire.text(), `"relay_secret_delivery"`) ||
		!strings.Contains(wire.text(), `"ciphertext"`) {
		t.Errorf("the relay traffic shows no sealed delivery, so the scan above proved nothing")
	}
	if !strings.Contains(string(logText), "answer ***") {
		t.Errorf("the run's log does not show the printed answer masked:\n%s", logText)
	}
}

// waitRelayTerminal polls until the run is terminal.
func waitRelayTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never finished (last %+v, %v)", id, r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lockedKeyUnlocked returns the passphrase-protected key unlocked, the form a tool receives.
func lockedKeyUnlocked(t *testing.T, locked string) string {
	t.Helper()
	unlocked, err := credential.UnlockSSHKey(locked, e2ePassphrase)
	if err != nil {
		t.Fatalf("UnlockSSHKey() error = %v", err)
	}
	return unlocked
}

// keyFingerprint names an unlocked private key by its public half.
func keyFingerprint(t *testing.T, key string) string {
	t.Helper()
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		t.Fatalf("the key does not parse: %v", err)
	}
	return ssh.FingerprintSHA256(signer.PublicKey())
}

// keyLines returns the base64 body lines of a PEM key that are secret on their own. The first is
// left out: an OpenSSH key's first line is a header every key of its type shares.
func keyLines(key string) []string {
	var out []string
	body := 0
	for _, line := range strings.Split(key, "\n") {
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		if body++; body > 1 && len(line) >= 16 {
			out = append(out, line)
		}
	}
	return out
}

// lockedBuffer is a log destination safe to write from many goroutines and read from the test.
type lockedBuffer struct {
	// mu guards buf.
	mu sync.Mutex
	// buf holds everything written.
	buf bytes.Buffer
}

// Write appends p.
func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Sync has nothing to flush.
func (l *lockedBuffer) Sync() error { return nil }

// String returns everything written so far.
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// encodedLogger returns a debug logger that encodes each entry to JSON the moment it is written, as
// a production log does, and the buffer it writes to. An observing core would keep references to
// the logged values instead, so a value wiped after it was logged would read back as never logged
// and a leak would pass unseen.
func encodedLogger() (*zap.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), buf,
		zapcore.DebugLevel)
	return zap.New(core), buf
}
