package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// Values the exit path tests deliver, looked for wherever they must not remain.
const (
	// exitKey is the delivered SSH key.
	exitKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nexit-path-key-body-3c9d\n" +
		"-----END OPENSSH PRIVATE KEY-----\n"
	// exitAnswer is the delivered survey answer.
	exitAnswer = "exit-path-answer-e5a1"
)

// exitRunner stands in for a tool on a relay worker. It records the credential files it was handed,
// checks they exist while it runs, prints the delivered secrets the way a careless play would, and
// then ends the way it is told to.
type exitRunner struct {
	// end is ok, fail, error, or block.
	end string
	// mu guards seen and missing.
	mu sync.Mutex
	// seen holds every credential path the tool was handed.
	seen []string
	// missing holds any handed path that did not exist while the tool ran.
	missing []string
	// started is signaled once the tool is running.
	started chan struct{}
}

// Run records the run's credential files and ends as configured.
func (e *exitRunner) Run(ctx context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	paths := append([]string{spec.PrivateKeyPath}, spec.ExtraVarsFiles...)
	paths = append(paths, spec.CredentialFiles...)
	e.mu.Lock()
	for _, p := range paths {
		if p == "" {
			continue
		}
		e.seen = append(e.seen, p)
		if _, err := os.Stat(p); err != nil {
			e.missing = append(e.missing, p)
		}
	}
	e.mu.Unlock()
	if spec.PrivateKeyPath != "" {
		if raw, err := os.ReadFile(spec.PrivateKeyPath); err == nil {
			_, _ = fmt.Fprintf(out, "key: %s\n", raw)
		}
	}
	_, _ = fmt.Fprintf(out, "answer: %v\n", spec.ExtraVars["db_password"])
	e.started <- struct{}{}
	switch e.end {
	case "fail":
		return roundhouse.Result{ExitCode: 2}, nil
	case "error":
		return roundhouse.Result{ExitCode: -1}, errors.New("the tool could not start")
	case "block":
		<-ctx.Done()
		return roundhouse.Result{ExitCode: -1}, ctx.Err()
	default:
		return roundhouse.Result{ExitCode: 0}, nil
	}
}

// handed returns the paths the tool was handed and any that were missing while it ran.
func (e *exitRunner) handed() (seen, missing []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...), append([]string(nil), e.missing...)
}

// exitStore wraps a run store to put a run on one of the exit paths a store decides: losing the
// fence at start, finding a cancel requested before start, or losing the lease while running.
type exitStore struct {
	// Store is the run store every call not overridden here reaches.
	run.Store
	// loseStart makes the fenced start report the run no longer pending.
	loseStart bool
	// cancelAtStart makes a running run read as having a cancel requested.
	cancelAtStart bool
	// loseLease makes every heartbeat report the lease gone.
	loseLease bool
}

// StartClaimed loses the fence when told to.
func (s *exitStore) StartClaimed(ctx context.Context, id, owner, secret string, at time.Time) (bool, error) {
	if s.loseStart {
		return false, nil
	}
	return s.Store.StartClaimed(ctx, id, owner, secret, at)
}

// Get reports a cancel on a running run when told to.
func (s *exitStore) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if err == nil && s.cancelAtStart && r.Status == run.StatusRunning {
		r.CancelRequested = true
	}
	return r, err
}

// Heartbeat reports the lease gone when told to.
func (s *exitStore) Heartbeat(ctx context.Context, id, owner string) error {
	if s.loseLease {
		return run.ErrNotFound
	}
	return s.Store.Heartbeat(ctx, id, owner)
}

// exitPayload is what the control node delivered for a run on the exit path tests.
func exitPayload(runID string, credentials ...*handoff.Credential) *handoff.Payload {
	p := &handoff.Payload{RunID: runID, Answers: map[string]string{"db_password": exitAnswer}}
	for _, c := range credentials {
		p.CredentialIDs = append(p.CredentialIDs, c.Record.ID)
		p.Credentials = append(p.Credentials, c)
	}
	return p
}

// exitKeyCredential is the delivered SSH key credential.
func exitKeyCredential() *handoff.Credential {
	return handoff.NewCredential(&credential.Credential{ID: "cred_ssh", Name: "fleet key",
		Kind: credential.KindSSHKey}, exitKey)
}

// TestDeliveredSecretsAreGoneOnEveryExitPath pins the cleanup guarantee on a relay worker. However
// a run ends, nothing delivered for it survives: the run directory its secrets were written into is
// removed, the opened payload is wiped, and the claim's delivery is discarded. That holds for the
// runs that finish, fail, error, are canceled, time out, lose their lease, or are interrupted by a
// shutdown, and for the runs that end before ever needing a secret: a setup failure, a lost start
// fence, a refused spec binding, and a cancel that arrived before the start.
//
//nolint:funlen // Test function.
func TestDeliveredSecretsAreGoneOnEveryExitPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		End          string
		Store        exitStore
		Tweak        func(*run.Run)
		Payload      func(string) *handoff.Payload
		Act          func(d *Dispatcher, id string)
		WantStatus   run.Status
		WantReceived bool
		WantRan      bool
	}{{ // Test 0: The run succeeds.
		Name: "succeeded", End: "ok", WantStatus: run.StatusSucceeded, WantReceived: true,
		WantRan: true,
	}, { // Test 1: The tool exits nonzero.
		Name: "tool failed", End: "fail", WantStatus: run.StatusFailed, WantReceived: true,
		WantRan: true,
	}, { // Test 2: The tool could not start.
		Name: "runner error", End: "error", WantStatus: run.StatusFailed, WantReceived: true,
		WantRan: true,
	}, { // Test 3: A person cancels it while it runs.
		Name: "canceled", End: "block", Act: func(d *Dispatcher, id string) { d.Cancel(id) },
		WantStatus: run.StatusCanceled, WantReceived: true, WantRan: true,
	}, { // Test 4: It runs past its timeout.
		Name: "timed out", End: "block", Tweak: func(r *run.Run) { r.Timeout = 1 },
		WantStatus: run.StatusFailed, WantReceived: true, WantRan: true,
	}, { // Test 5: The worker shuts down under it.
		Name: "shut down", End: "block", Act: func(d *Dispatcher, _ string) { go d.Close() },
		WantStatus: run.StatusInterrupted, WantReceived: true, WantRan: true,
	}, { // Test 6: The worker loses its lease while it runs.
		Name: "lease lost", End: "block", Store: exitStore{loseLease: true},
		WantStatus: run.StatusInterrupted, WantReceived: true, WantRan: true,
	}, { // Test 7: Setup fails before any secret is needed.
		Name: "setup failed early", Tweak: func(r *run.Run) { r.InventoryID = "inv_absent" },
		WantStatus: run.StatusFailed, WantReceived: false, WantRan: false,
	}, { // Test 8: Setup fails after the answers were opened and before anything was written.
		Name: "setup failed after opening", Tweak: func(r *run.Run) { r.ProjectID = "proj_absent" },
		WantStatus: run.StatusFailed, WantReceived: true, WantRan: false,
	}, { // Test 9: Setup fails after the run directory already holds a written key.
		Name:  "setup failed late",
		Tweak: func(r *run.Run) { r.CredentialIDs = []string{"cred_ssh", "cred_bad"} },
		Payload: func(id string) *handoff.Payload {
			return exitPayload(id, exitKeyCredential(), handoff.NewCredential(&credential.Credential{
				ID: "cred_bad", Name: "bad env", Kind: credential.KindEnv}, "not a pair"))
		},
		WantStatus: run.StatusFailed, WantReceived: true, WantRan: false,
	}, { // Test 10: Another worker won the start fence.
		Name: "start lost", Store: exitStore{loseStart: true}, WantStatus: run.StatusPending,
		WantReceived: false, WantRan: false,
	}, { // Test 11: The spec moved after it was approved.
		Name: "binding refused", Tweak: func(r *run.Run) { r.ApprovedSpecBinding = "not-this-spec" },
		WantStatus: run.StatusFailed, WantReceived: false, WantRan: false,
	}, { // Test 12: A cancel was requested before the tool started.
		Name: "canceled before start", Store: exitStore{cancelAtStart: true},
		WantStatus: run.StatusCanceled, WantReceived: false, WantRan: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_exit_%d", testNum)
			held := newHeldDeliveries()
			payload := exitPayload(id, exitKeyCredential())
			if test.Payload != nil {
				payload = test.Payload(id)
			}
			held.payloads[id] = payload
			runner := &exitRunner{end: test.End, started: make(chan struct{}, 1)}
			store := test.Store
			store.Store = run.NewMemStore()
			root := filepath.Join(t.TempDir(), "runfiles")
			d := New(&store, runner, zap.NewNop(), WithNoJanitor(), WithClaimInterval(time.Millisecond),
				WithSecretDelivery(held), WithRunFilesRoot(root))
			t.Cleanup(d.Close)
			r := &run.Run{ID: id, Tool: run.ToolBash, Command: "deploy", Status: run.StatusPending,
				CredentialIDs: []string{"cred_ssh"}, SealedNames: []string{"db_password"},
				CreatedAt: time.Now()}
			if test.Tweak != nil {
				test.Tweak(r)
			}
			if err := store.Save(context.Background(), r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if test.Act != nil {
				select {
				case <-runner.started:
				case <-time.After(waitBudget):
					t.Fatalf("the tool never started")
				}
				test.Act(d, id)
			}
			// The long budget, not waitFor's: the lease-lost path waits out a heartbeat tick, which a
			// loaded race run can stretch well past a few seconds.
			deadline := time.Now().Add(waitBudget)
			for _, discarded := held.counts(id); discarded == 0; _, discarded = held.counts(id) {
				if time.Now().After(deadline) {
					t.Fatalf("the run's delivery was never discarded")
				}
				time.Sleep(5 * time.Millisecond)
			}

			received, _ := held.counts(id)
			if (received > 0) != test.WantReceived {
				t.Errorf("delivery received %d times, want received %v", received, test.WantReceived)
			}
			if handed, wiped := held.wiped(id); handed && !wiped {
				t.Errorf("the opened delivery was not wiped when the run ended")
			}
			seen, missing := runner.handed()
			if (len(seen) > 0) != test.WantRan {
				t.Errorf("the tool was handed %v, want it to have run: %v", seen, test.WantRan)
			}
			if len(missing) > 0 {
				t.Errorf("credential files missing while the tool ran: %v", missing)
			}
			for _, p := range seen {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s survived the run (stat error %v)", p, err)
				}
			}
			if dirs := runDirsUnder(t, root); len(dirs) != 0 {
				t.Errorf("run directories survived the run: %v", dirs)
			}
			got, err := store.Store.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.WantStatus {
				t.Errorf("status = %s (%s), want %s", got.Status, got.Error, test.WantStatus)
			}
			logs, err := store.Log(context.Background(), id)
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			for _, secret := range []string{"exit-path-key-body", exitAnswer} {
				if strings.Contains(string(logs), secret) || strings.Contains(got.Error, secret) {
					t.Errorf("the run's record carries %q", secret)
				}
			}
		})
	}
}

// runDirsUnder lists the run directories left under root.
func runDirsUnder(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestARelayWorkerSweepsWhatACrashLeftBehind pins the one exit path no deferred cleanup can reach:
// a worker process killed mid-run. Its run directory is left on disk, unlocked, and the sweep every
// executor on the host runs reclaims it. A relay worker holds no credential store, so before
// delivery it never swept, and with delivery it writes into the same directories, so it must.
//
// The directory is built the way a run leaves it, with its lock and heartbeat counter, and the test
// waits for the relay worker's startup pass, which the sweep lock in the root records. That pass
// does not delete on its own: a directory goes only when a second pass, a full gap later on the
// sweeper's monotonic clock, finds it unchanged and still unlocked, which the runfiles package
// proves with a clock it drives. So the crashed directory is still there after the startup pass,
// and is now under the sweeper's watch.
func TestARelayWorkerSweepsWhatACrashLeftBehind(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runfiles")
	dead := filepath.Join(root, "run-crashed-1234")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	// The lock file the crashed process held is left behind unlocked, as the operating system
	// releases a dead process's locks, beside the heartbeat counter it stopped advancing.
	for name, body := range map[string]string{".lock": "", ".beat": "17", "cred-1": exitKey} {
		if err := os.WriteFile(filepath.Join(dead, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	sweepLock := filepath.Join(root, ".sweep")
	if _, err := os.Stat(sweepLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the root was swept before the worker started, so this proves nothing: %v", err)
	}
	d := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor(),
		WithSecretDelivery(newHeldDeliveries()), WithRunFilesRoot(root))
	t.Cleanup(d.Close)
	deadline := time.Now().Add(waitBudget)
	for {
		if _, err := os.Stat(sweepLock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a relay worker started without sweeping its run files root")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(dead); err != nil {
		t.Errorf("the startup pass deleted the crashed run's directory at first sight: %v", err)
	}
}

// wipeCheckingRunner reports, while the tool runs, whether the run's opened delivery was already
// wiped.
type wipeCheckingRunner struct {
	// held is the delivery source the run's secrets came from.
	held *heldDeliveries
	// id is the run whose delivery is checked.
	id string
	// result receives whether the delivery was wiped before the tool started.
	result chan bool
}

// Run checks the delivery and succeeds.
func (w *wipeCheckingRunner) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	handed, wiped := w.held.wiped(w.id)
	w.result <- handed && wiped
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestTheOpenedCopyIsDroppedBeforeTheToolStarts pins that a relay worker holds a run's opened
// secrets in memory only until they are written where the tool reads them. A tool that runs for
// hours does not keep a second, in-memory copy of every credential alive beside it.
func TestTheOpenedCopyIsDroppedBeforeTheToolStarts(t *testing.T) {
	t.Parallel()
	const id = "run_wipe_early"
	held := newHeldDeliveries()
	held.payloads[id] = exitPayload(id, exitKeyCredential())
	runner := &wipeCheckingRunner{held: held, id: id, result: make(chan bool, 1)}
	store := run.NewMemStore()
	d := New(store, runner, zap.NewNop(), WithNoJanitor(), WithClaimInterval(time.Millisecond),
		WithSecretDelivery(held), WithRunFilesRoot(filepath.Join(t.TempDir(), "runfiles")))
	t.Cleanup(d.Close)
	if err := store.Save(context.Background(), &run.Run{ID: id, Tool: run.ToolBash, Command: "deploy",
		Status: run.StatusPending, CredentialIDs: []string{"cred_ssh"},
		SealedNames: []string{"db_password"}, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	select {
	case wiped := <-runner.result:
		if !wiped {
			t.Errorf("the opened delivery was still held when the tool started")
		}
	case <-time.After(waitBudget):
		t.Fatalf("the tool never started")
	}
	if got := waitTerminal(t, store, id); got.Status != run.StatusSucceeded {
		t.Errorf("status = %s (%s), want succeeded", got.Status, got.Error)
	}
}
