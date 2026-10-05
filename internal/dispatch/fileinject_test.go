package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// fileSecret is the token inside the kubeconfig a file-injecting credential writes. It must never
// appear anywhere but the written file.
const fileSecret = "kube-token-0f1e2d3c4b5a"

// fileKubeconfig is the multiline field value a custom kubeconfig type renders into its file.
const fileKubeconfig = "apiVersion: v1\nkind: Config\nusers:\n- name: deployer\n  user:\n" +
	"    token: " + fileSecret + "\n"

// fileFixture is a dispatcher's credential stores holding one credential of a custom type that
// writes a kubeconfig file and hands its path over as KUBECONFIG and as an extra var.
type fileFixture struct {
	// creds holds the typed credential.
	creds credential.Store
	// types holds the custom type.
	types credential.TypeStore
	// sealer seals and opens the credential.
	sealer *credential.Sealer
	// root is where the run credential directories are created.
	root string
}

// newFileFixture builds the stores and a private root for one test.
func newFileFixture(t *testing.T) *fileFixture {
	t.Helper()
	ctx := context.Background()
	fx := &fileFixture{
		creds: credential.NewMemStore(), types: credential.NewMemTypeStore(),
		sealer: credential.NewSealer("pass", "salt"), root: filepath.Join(t.TempDir(), "runfiles"),
	}
	typ := &credential.CredentialType{
		ID: "ctype_kube", Name: "Kubeconfig",
		Fields: []credential.Field{
			{Name: "kubeconfig", Secret: true, Multiline: true},
			{Name: "context"},
		},
		FileInjectors: map[string]string{"template": "{{ kubeconfig }}"},
		EnvInjectors: map[string]string{
			"KUBECONFIG": "{{ tower.filename }}", "KUBE_CONTEXT": "{{context}}",
		},
		ExtraVarInjectors: map[string]string{"k8s_kubeconfig": "{{ awx.filename }}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := fx.types.Save(ctx, typ); err != nil {
		t.Fatal(err)
	}
	fields, err := json.Marshal(map[string]string{"kubeconfig": fileKubeconfig, "context": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := fx.sealer.Seal(string(fields))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.creds.Save(ctx, &credential.Credential{
		ID: "cred_kube", Name: "prod-kube", TypeID: "ctype_kube", Secret: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	return fx
}

// options returns the dispatcher options that wire the fixture in.
func (fx *fileFixture) options() []Option {
	return []Option{
		WithCredentials(fx.creds, fx.sealer), WithCredentialTypes(fx.types),
		WithRunFilesRoot(fx.root), WithNotifyClient(http.DefaultClient),
	}
}

// runDirs lists the run directories left under the fixture's root. The sweep's own lock file sits
// beside them and is not one.
func (fx *fileFixture) runDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fx.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// fileObservation is what the tool saw of the credential while it ran.
type fileObservation struct {
	// path is the KUBECONFIG value the tool received.
	path string
	// content is what the file held.
	content string
	// fileMode and dirMode are the file's and its directory's permissions.
	fileMode, dirMode fs.FileMode
	// inRoot reports that the file's directory sits directly under the configured root.
	inRoot bool
	// mounted reports that the path is in spec.CredentialFiles, so a container run mounts it.
	mounted bool
	// extraVar is the k8s_kubeconfig extra var the play received through its vars file.
	extraVar string
	// context is the KUBE_CONTEXT value.
	context string
}

// observeFile reads what the spec hands the tool and checks it on disk.
func observeFile(spec roundhouse.Spec, root string) (fileObservation, error) {
	var obs fileObservation
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "KUBECONFIG="); ok {
			obs.path = v
		}
		if v, ok := strings.CutPrefix(kv, "KUBE_CONTEXT="); ok {
			obs.context = v
		}
	}
	if obs.path == "" {
		return obs, errors.New("no KUBECONFIG in the environment")
	}
	body, err := os.ReadFile(obs.path)
	if err != nil {
		return obs, err
	}
	obs.content = string(body)
	fi, err := os.Stat(obs.path)
	if err != nil {
		return obs, err
	}
	di, err := os.Stat(filepath.Dir(obs.path))
	if err != nil {
		return obs, err
	}
	obs.fileMode, obs.dirMode = fi.Mode().Perm(), di.Mode().Perm()
	obs.inRoot = filepath.Dir(filepath.Dir(obs.path)) == root
	for _, f := range spec.CredentialFiles {
		obs.mounted = obs.mounted || f == obs.path
	}
	for _, vf := range spec.ExtraVarsFiles {
		raw, err := os.ReadFile(vf)
		if err != nil {
			return obs, err
		}
		var vars map[string]string
		if json.Unmarshal(raw, &vars) == nil && vars["k8s_kubeconfig"] != "" {
			obs.extraVar = vars["k8s_kubeconfig"]
		}
	}
	return obs, nil
}

// finalizeProbe is a run store that records, at the instant a run is recorded as finished, which run
// credential directories still exist, so a test can prove the files were gone before anyone could see
// the run had ended rather than merely soon after.
type finalizeProbe struct {
	run.Store
	// root is the run credential directory root to inspect.
	root string
	// mu guards left.
	mu sync.Mutex
	// left holds the directories present when the run was finalized.
	left []string
}

// FinalizeRunning records the run directories under root, then finalizes. The sweep's own lock
// file sits beside them once a sweep has run, and is not a run's.
func (p *finalizeProbe) FinalizeRunning(ctx context.Context, id string, fin run.Finalization) (bool, error) {
	entries, _ := os.ReadDir(p.root)
	p.mu.Lock()
	for _, e := range entries {
		if e.IsDir() {
			p.left = append(p.left, e.Name())
		}
	}
	p.mu.Unlock()
	return p.Store.FinalizeRunning(ctx, id, fin)
}

// leftAtFinalize returns the directories present when the run was finalized.
func (p *finalizeProbe) leftAtFinalize() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.left...)
}

// waitStarted waits for the tool to start, failing at once with the run's error when the run ends
// without the tool ever starting, rather than hanging until the test binary times out.
func waitStarted(t *testing.T, store run.Store, id string, started <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for {
		select {
		case <-started:
			return
		case <-time.After(5 * time.Millisecond):
		}
		if r, err := store.Get(context.Background(), id); err == nil && r.Status.Terminal() {
			// A tool that signals and returns at once can finish inside one poll interval, so the
			// signal is checked again before an ended run is read as one that never started.
			select {
			case <-started:
				return
			default:
			}
			t.Fatalf("run %s ended %s before the tool started: %s", id, r.Status, r.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never started its tool", id)
		}
	}
}

// TestFileInjectorEveryExitPath runs a credential that writes a kubeconfig through every way a run
// can end and proves, for each, that the tool received a private file at the path its injectors
// named, that the file is gone afterwards, and that the secret inside it reached neither the run's
// log, its stored record, the audit chain, nor the server log.
func TestFileInjectorEveryExitPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Mode       string
		WantStatus run.Status
	}{{ // Test 0: The tool succeeds.
		Mode: "succeed", WantStatus: run.StatusSucceeded,
	}, { // Test 1: The tool fails with a nonzero exit.
		Mode: "fail", WantStatus: run.StatusFailed,
	}, { // Test 2: An operator cancels the run while the tool is running.
		Mode: "cancel", WantStatus: run.StatusCanceled,
	}, { // Test 3: The run outlasts its timeout.
		Mode: "timeout", WantStatus: run.StatusFailed,
	}, { // Test 4: The runner itself returns an error.
		Mode: "error", WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fx := newFileFixture(t)
			core, logs := observer.New(zapcore.DebugLevel)
			store := &finalizeProbe{Store: run.NewMemStore(), root: fx.root}
			audits := audit.NewMemStore()

			var (
				mu      sync.Mutex
				obs     fileObservation
				obsErr  error
				started = make(chan struct{})
			)
			runner := roundhouse.RunnerFunc(func(ctx context.Context, spec roundhouse.Spec,
				out io.Writer) (roundhouse.Result, error) {
				o, err := observeFile(spec, fx.root)
				mu.Lock()
				obs, obsErr = o, err
				mu.Unlock()
				// A tool that prints its kubeconfig, as kubectl config view --raw does.
				_, _ = io.WriteString(out, "config:\n"+o.content+"done\n")
				close(started)
				switch test.Mode {
				case "fail":
					return roundhouse.Result{ExitCode: 2}, nil
				case "cancel", "timeout":
					<-ctx.Done()
					return roundhouse.Result{ExitCode: -1}, ctx.Err()
				case "error":
					return roundhouse.Result{ExitCode: -1}, errors.New("runner broke")
				}
				return roundhouse.Result{ExitCode: 0}, nil
			})
			opts := append(fx.options(), WithAudits(audits))
			if test.Mode == "timeout" {
				opts = append(opts, WithRunTimeout(200*time.Millisecond))
			}
			d := New(store, runner, zap.New(core), opts...)
			defer d.Close()

			r, err := d.Submit(context.Background(), "site.yml", "hosts.ini",
				run.WithCredentialIDs([]string{"cred_kube"}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			waitStarted(t, store, r.ID, started)
			if test.Mode == "cancel" && !d.Cancel(r.ID) {
				t.Fatal("Cancel() returned false for a running run")
			}
			got := waitTerminal(t, store, r.ID)
			if got.Status != test.WantStatus {
				t.Errorf("status = %q, want %q", got.Status, test.WantStatus)
			}

			mu.Lock()
			defer mu.Unlock()
			if obsErr != nil {
				t.Fatalf("the tool could not use the credential file: %v", obsErr)
			}
			if obs.content != fileKubeconfig {
				t.Errorf("file content = %q, want the rendered kubeconfig", obs.content)
			}
			if runtime.GOOS != "windows" && (obs.fileMode != 0o600 || obs.dirMode != 0o700) {
				t.Errorf("file mode %v in directory mode %v, want 0600 in 0700", obs.fileMode, obs.dirMode)
			}
			if !obs.inRoot || !obs.mounted {
				t.Errorf("file in run directory under root = %v, mounted for a container = %v, want both",
					obs.inRoot, obs.mounted)
			}
			if obs.extraVar != obs.path || obs.context != "prod" {
				t.Errorf("extra var = %q, context = %q, want the path %q and prod", obs.extraVar,
					obs.context, obs.path)
			}
			if _, err := os.Stat(obs.path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("credential file survived a run that ended by %s: %v", test.Mode, err)
			}
			if left := store.leftAtFinalize(); len(left) > 0 {
				t.Errorf("run directories still present when the run that ended by %s was recorded "+
					"as finished: %v", test.Mode, left)
			}
			if left := fx.runDirs(t); len(left) > 0 {
				t.Errorf("run directories left after a run that ended by %s: %v", test.Mode, left)
			}
			assertNoFileSecret(t, store, audits, logs, r.ID)
		})
	}
}

// assertNoFileSecret proves the token inside the written file reached none of the places a run
// leaves a record: its stored log, its stored record, the audit chain, and the server's own log.
func assertNoFileSecret(t *testing.T, store run.Store, audits audit.Store, logs *observer.ObservedLogs,
	id string) {
	t.Helper()
	ctx := context.Background()
	body, err := store.Log(ctx, id)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if strings.Contains(string(body), fileSecret) {
		t.Errorf("the file's secret reached the run log: %q", body)
	}
	if !strings.Contains(string(body), maskToken) {
		t.Errorf("the run log does not show the secret masked: %q", body)
	}
	r, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), fileSecret) || strings.Contains(string(record), "kind: Config") {
		t.Errorf("the file reached the stored run record: %s", record)
	}
	entries, err := audits.Chain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chain), fileSecret) {
		t.Error("the file's secret reached the audit chain")
	}
	for _, e := range logs.All() {
		line, _ := json.Marshal(e.ContextMap())
		if strings.Contains(e.Message, fileSecret) || strings.Contains(string(line), fileSecret) {
			t.Errorf("the file's secret reached the server log: %s %s", e.Message, line)
		}
	}
}

// TestFileInjectorCleansUpAFailedMaterialization proves a run whose second credential fails after
// the first wrote its file leaves no file behind: the cleanup materializeCredentials returns with
// the error removes the run's whole directory.
func TestFileInjectorCleansUpAFailedMaterialization(t *testing.T) {
	t.Parallel()
	fx := newFileFixture(t)
	sealed, err := fx.sealer.Seal("line-one\nLD_PRELOAD=/evil.so")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.creds.Save(context.Background(), &credential.Credential{
		ID: "cred_bad", Name: "bad", Kind: credential.KindToken, Secret: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{credentials: fx.creds, credentialTypes: fx.types, sealer: fx.sealer,
		runFilesRoot: fx.root}
	spec := &roundhouse.Spec{}
	cleanup, _, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_bad", CredentialIDs: []string{"cred_kube", "cred_bad"}}, spec)
	if err == nil {
		cleanup()
		t.Fatal("materializeCredentials() accepted a token spanning two lines")
	}
	if len(spec.CredentialFiles) != 1 {
		cleanup()
		t.Fatalf("the typed credential's file was not written first: %v", spec.CredentialFiles)
	}
	if _, err := os.Stat(spec.CredentialFiles[0]); err != nil {
		cleanup()
		t.Fatalf("the file is missing before cleanup, so this proves nothing: %v", err)
	}
	cleanup()
	if left := fx.runDirs(t); len(left) > 0 {
		t.Errorf("run directories left after a failed materialization: %v", left)
	}
}

// TestDispatcherStartSweepsACrashedRun proves the recovery for a process killed mid-run, which runs
// no cleanup: the next dispatcher to start on the host sweeps its root at once and puts the
// directory the dead run left under watch, while a run another dispatcher on the same host is still
// executing keeps its file. The startup sweep does not delete on its own: a directory goes only
// once a second sweep, a full gap later on the sweeper's monotonic clock, finds it unchanged, which
// the runfiles package proves with a clock it drives. A restart that deleted at first sight would
// skip the rule's second look at a lock it read once.
func TestDispatcherStartSweepsACrashedRun(t *testing.T) {
	t.Parallel()
	fx := newFileFixture(t)

	// A live run on another dispatcher sharing the host, blocked inside its tool.
	store := run.NewMemStore()
	started := make(chan string, 1)
	release := make(chan struct{})
	runner := roundhouse.RunnerFunc(func(ctx context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		o, _ := observeFile(spec, fx.root)
		started <- o.path
		select {
		case <-release:
		case <-ctx.Done():
		}
		return roundhouse.Result{ExitCode: 0}, nil
	})
	busy := New(store, runner, nil, fx.options()...)
	defer busy.Close()
	r, err := busy.Submit(context.Background(), "site.yml", "hosts.ini",
		run.WithCredentialIDs([]string{"cred_kube"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	var livePath string
	select {
	case livePath = <-started:
	case <-time.After(waitBudget):
		t.Fatal("the live run never started its tool")
	}
	if livePath == "" {
		t.Fatal("the live run received no credential file")
	}
	// What a killed executor leaves: a run directory with its lock file and a credential file, and
	// no process holding the lock.
	crashed := filepath.Join(fx.root, "run-run_dead-123")
	if err := os.MkdirAll(crashed, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".lock", "cred-1"} {
		if err := os.WriteFile(filepath.Join(crashed, name), []byte(fileKubeconfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(crashed); err != nil {
		t.Fatalf("the crashed run's directory is already gone, so this proves nothing: %v", err)
	}

	sweepLock := filepath.Join(fx.root, ".sweep")
	if err := os.Remove(sweepLock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	restarted := New(run.NewMemStore(), quietRunner{}, nil, fx.options()...)
	defer restarted.Close()
	deadline := time.Now().Add(waitBudget)
	for {
		if _, err := os.Stat(sweepLock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restarted dispatcher never swept its root")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := os.Stat(crashed); err != nil {
		t.Errorf("the startup sweep deleted the crashed run's directory at first sight: %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("a dispatcher start removed a live run's credential file: %v", err)
	}
	close(release)
	waitTerminal(t, store, r.ID)
	if diff := cmp.Diff([]string{"run-run_dead-123"}, fx.runDirs(t)); diff != "" {
		t.Errorf("run directories left after the live run finished (-want +got):\n%s", diff)
	}
}

// TestKubeconfigKindReachesTheTool proves the built-in kubeconfig kind end to end: the document is
// written to a private file bound to all three variables, its token is masked when a tool prints
// the file, its ordinary lines are not, and the file is gone after the run.
func TestKubeconfigKindReachesTheTool(t *testing.T) {
	t.Parallel()
	fx := newFileFixture(t)
	sealed, err := fx.sealer.Seal(fileKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.creds.Save(context.Background(), &credential.Credential{
		ID: "cred_kc", Name: "kc", Kind: credential.KindKubeconfig, Secret: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	store := run.NewMemStore()
	var paths []string
	var mu sync.Mutex
	runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		out io.Writer) (roundhouse.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, name := range credential.KubeconfigEnvVars {
			for _, kv := range spec.Env {
				if v, ok := strings.CutPrefix(kv, name+"="); ok {
					paths = append(paths, v)
				}
			}
		}
		if len(paths) > 0 {
			body, _ := os.ReadFile(paths[0])
			_, _ = out.Write(body)
		}
		return roundhouse.Result{ExitCode: 0}, nil
	})
	d := New(store, runner, nil, fx.options()...)
	defer d.Close()
	r, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("kubectl config view --raw"), run.WithCredentialIDs([]string{"cred_kc"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, r.ID); got.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", got.Status, got.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 || paths[0] != paths[1] || paths[1] != paths[2] {
		t.Fatalf("paths = %v, want one file bound to all three variables", paths)
	}
	if _, err := os.Stat(paths[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("kubeconfig file survived the run: %v", err)
	}
	body, err := store.Log(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), fileSecret) || !strings.Contains(string(body), "kind: Config") {
		t.Errorf("log = %q, want the token masked and the structure readable", body)
	}
}
