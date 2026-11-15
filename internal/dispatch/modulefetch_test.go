package dispatch

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// registryHost is the registry host the registry-address configurations name. It resolves
// nowhere, so only a stand-in get ever installs a module from it.
const registryHost = "registry.example.test"

// registryURLMark stands for the test registry's address inside a module's files, replaced when
// the archive is served, so a module can call another module from the same server.
const registryURLMark = "@REGISTRY@"

// registryModule is one module the test registry serves, as an archive at
// /archive/<name>-1.0.0.tar.gz.
type registryModule struct {
	// Files are the module's files by path.
	Files map[string]string
	// Hang makes its download never answer, until the client gives up.
	Hang bool
	// Pad adds a file of this many bytes to its archive.
	Pad int
}

// testRegistry serves the modules it holds as tar.gz archives, each at an address a configuration
// names directly and the gate's download reaches with no configuration and no credential. A test
// can change what is served at a module's address while the test runs.
type testRegistry struct {
	// srv is the HTTP server.
	srv *httptest.Server
	// modules are the modules served, by name.
	modules map[string]registryModule
	// mu guards downloads and replaced.
	mu sync.Mutex
	// downloads counts each module's archive downloads.
	downloads map[string]int
	// replaced holds the files served in place of a module's own, by name.
	replaced map[string]map[string]string
}

// newTestRegistry starts a registry serving modules and stops it when the test ends.
func newTestRegistry(t *testing.T, modules map[string]registryModule) *testRegistry {
	t.Helper()
	reg := &testRegistry{modules: modules, downloads: map[string]int{},
		replaced: map[string]map[string]string{}}
	reg.srv = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.srv.Close)
	return reg
}

// replace serves files at the named module's address from now on, in place of what it served.
func (reg *testRegistry) replace(name string, files map[string]string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.replaced[name] = files
}

// serve answers with a module's archive.
func (reg *testRegistry) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "archive":
		name, _, _ := strings.Cut(strings.TrimSuffix(parts[1], ".tar.gz"), "-")
		mod, ok := reg.modules[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reg.mu.Lock()
		if files, replaced := reg.replaced[name]; replaced {
			mod = registryModule{Files: files}
		}
		reg.mu.Unlock()
		archive := moduleArchive(registryModule{Files: reg.expand(mod.Files), Pad: mod.Pad})
		w.Header().Set("Content-Type", "application/gzip")
		// The downloader asks the archive's size first, and only a GET downloads it.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
			return
		}
		reg.mu.Lock()
		reg.downloads[name]++
		reg.mu.Unlock()
		if mod.Hang {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(archive)
	default:
		http.NotFound(w, r)
	}
}

// expand returns files with registryURLMark replaced by the registry's address.
func (reg *testRegistry) expand(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for name, body := range files {
		out[name] = strings.ReplaceAll(body, registryURLMark, reg.srv.URL)
	}
	return out
}

// downloadsOf returns how many times a module's archive was downloaded.
func (reg *testRegistry) downloadsOf(name string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.downloads[name]
}

// call is a configuration calling the named module by its archive's address on the registry,
// which the gate's download reaches with no configuration and no credential.
func (reg *testRegistry) call(name string) string {
	return urlCall(reg.srv.URL, name)
}

// moduleArchive packs a module's files into a tar.gz archive, with a padding file when it asks for
// one.
func moduleArchive(mod registryModule) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, content []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)),
			Typeflag: tar.TypeReg})
		_, _ = tw.Write(content)
	}
	for name, body := range mod.Files {
		add(name, []byte(body))
	}
	if mod.Pad > 0 {
		add("padding.bin", bytes.Repeat([]byte("x"), mod.Pad))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fetchingRunner executes nothing and downloads modules with the real runner, so the gate's
// download is the real tool's while every run a test submits stays a no-op.
type fetchingRunner struct {
	// Runner executes the runs a test submits.
	roundhouse.Runner
	// fetch is the real runner's module download.
	fetch roundhouse.ModuleFetcher
}

// FetchModules downloads with the real runner.
func (f fetchingRunner) FetchModules(ctx context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result,
	error) {
	return f.fetch.FetchModules(ctx, spec, out)
}

// newFetchingRunner returns a fetchingRunner, standing the test down where terraform cannot run.
func newFetchingRunner(t *testing.T) fetchingRunner {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		standDownOrFail(t, "terraform is not on PATH, so the gate's download cannot run")
	}
	fetch, ok := roundhouse.NewAnsibleRunner().(roundhouse.ModuleFetcher)
	if !ok {
		t.Fatal("the runner cannot fetch modules")
	}
	return fetchingRunner{Runner: dispatchtest.OKRunner(), fetch: fetch}
}

// registryCall is a configuration calling the named module from the test registry by its registry
// address.
func registryCall(name string) string {
	return fmt.Sprintf("module \"net\" {\n  source  = \"%s/acme/%s/null\"\n"+
		"  version = \"~> 1.0\"\n}\n", registryHost, name)
}

// urlCall is a configuration calling the named module by the address of its archive on the
// registry at base.
func urlCall(base, name string) string {
	return fmt.Sprintf("module \"net\" {\n  source = \"%s/archive/%s-1.0.0.tar.gz\"\n}\n", base,
		name)
}

// testModules are the modules the gate tests download.
func testModules() map[string]registryModule {
	return map[string]registryModule{
		"clean": {Files: map[string]string{"main.tf": "output \"o\" {\n  value = 1\n}\n"}},
		"hiding": {Files: map[string]string{
			"main.tf":        "module \"inner\" {\n  source = \"./nested\"\n}\n",
			"nested/main.tf": "data \"external\" \"x\" {\n  program = [\"true\"]\n}\n",
		}},
		"hangs": {Files: map[string]string{"main.tf": ""}, Hang: true},
		"huge":  {Files: map[string]string{"main.tf": ""}, Pad: 3 << 20},
		"loops": {Files: map[string]string{"main.tf": urlCall(registryURLMark, "loops")}},
	}
}

// TestTheGateDownloadsModulesBeforeItReadsAPlan drives the gate's module download through the
// dispatcher, with real terraform against a local module server.
//
// A fresh commit holds no downloaded modules, so a plan calling a remote module was held or refused
// whatever the module held. The gate downloads the modules first, with no credential and nothing of
// the server's environment but its search path and proxies, and reads what was installed. A clean
// module passes, one hiding an external data source is still caught by address, and a download
// that fails, hangs, grows too large, or loops leaves the plan unclassified with why.
//
//nolint:funlen // Test function.
func TestTheGateDownloadsModulesBeforeItReadsAPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Module is the module the configuration calls.
		Module string
		// WantClass is the scan's classification.
		WantClass string
		// WantFinding is a finding the scan must name, empty for none.
		WantFinding string
		// WantFetched is whether the download completed.
		WantFetched bool
		// WantReason is a fragment of why the download did not complete.
		WantReason string
		// Timeout bounds the download. Zero gives it a minute, so on a busy machine nothing but the
		// case about the time bound is stopped by it.
		Timeout time.Duration
	}{{ // Test 0: A clean module passes.
		Module: "clean", WantClass: run.DryRunChangeFree, WantFetched: true,
	}, { // Test 1: One hiding an external data source behind a nested module is still caught.
		Module: "hiding", WantClass: run.DryRunNotChangeFree, WantFetched: true,
		WantFinding: "module.net.module.inner.data.external.x runs a program during plan " +
			"(.terraform/modules/net/nested/main.tf line 1)",
	}, { // Test 2: A module the server does not have fails the download, which fails closed.
		Module: "absent", WantClass: run.DryRunIncomplete,
		WantReason: "the gate's terraform get failed with exit status 1",
	}, { // Test 3: A download that never answers is stopped at the time bound.
		Module: "hangs", WantClass: run.DryRunIncomplete,
		WantReason: "the gate's terraform get did not finish within 3s", Timeout: 3 * time.Second,
	}, { // Test 4: A download larger than the bound is stopped and not read.
		Module: "huge", WantClass: run.DryRunIncomplete,
		WantReason: "the gate's terraform get wrote more than 1 MiB",
	}, { // Test 5: A module that calls itself without end is bounded and fails closed.
		Module: "loops", WantClass: run.DryRunIncomplete,
		WantReason: "the gate's terraform get",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reg := newTestRegistry(t, testModules())
			dir := t.TempDir()
			dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": reg.call(test.Module)})
			store := run.NewMemStore()
			files := t.TempDir()
			d := New(store, newFetchingRunner(t), zap.NewNop(),
				WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
				WithRunFilesRoot(files), WithNoJanitor(),
				WithModuleFetchLimits(cmp.Or(test.Timeout, time.Minute), 1<<20))
			t.Cleanup(d.Close)

			got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
				run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if left, _ := filepath.Glob(filepath.Join(files, "run-modules-*")); len(left) != 0 {
				t.Errorf("the gate's private copy outlived the scan: %q", left)
			}
			if len(got.DryRunScans) != 1 {
				t.Fatalf("scans = %+v, want the gate's one scan", got.DryRunScans)
			}
			scan := got.DryRunScans[0]
			if scan.Classification != test.WantClass {
				t.Errorf("classification = %q, want %q (findings %q, unread %q, fetch %+v)",
					scan.Classification, test.WantClass, scan.Findings, scan.Unread, scan.Fetch)
			}
			if held := got.Status == run.StatusPendingApproval; held != (test.WantClass !=
				run.DryRunChangeFree) {
				t.Errorf("status = %q, want held exactly when the plan is not change free", got.Status)
			}
			if scan.Fetch == nil || scan.Fetch.Command != "terraform get" {
				t.Fatalf("fetch = %+v, want the gate's terraform get recorded", scan.Fetch)
			}
			if scan.Fetch.Fetched() != test.WantFetched {
				t.Errorf("fetched = %v, want %v: %+v", scan.Fetch.Fetched(), test.WantFetched, scan.Fetch)
			}
			if test.WantFinding != "" && !slices.Contains(scan.Findings, test.WantFinding) {
				t.Errorf("findings = %q, want %q", scan.Findings, test.WantFinding)
			}
			if test.WantFetched {
				if scan.Fetch.ExitStatus != 0 || !slices.Contains(scan.Inputs,
					".terraform/modules/net/main.tf") {
					t.Errorf("fetch %+v, inputs %q, want exit 0 and the downloaded module read",
						scan.Fetch, scan.Inputs)
				}
				return
			}
			if scan.Fetch.ExitStatus == 0 || !strings.Contains(scan.Fetch.Error, test.WantReason) {
				t.Errorf("fetch = %+v, want a failure saying %q", scan.Fetch, test.WantReason)
			}
			if !strings.Contains(strings.Join(scan.Unread, "\n"), "not downloaded, since "+
				test.WantReason) {
				t.Errorf("unread = %q, want the module unread with the download's failure", scan.Unread)
			}
		})
	}
}

// TestOneSubmissionDownloadsOnce covers the reuse of a download's scan. A submission asks its rules
// several times, each through the gate's scan, and a pull request review asks once more before it
// submits. Each of those must not download the modules again.
func TestOneSubmissionDownloadsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, testModules())
	dir := t.TempDir()
	dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": reg.call("clean")})
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(),
		WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
		WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	probe := &run.Run{Tool: run.ToolTerraform, Command: dir, DryRun: true}
	if scans := d.DryRunScansAt(ctx, probe); !run.ScansChangeFree(scans) {
		t.Fatalf("the probe's scan = %+v, want change free", scans)
	}
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.Status == run.StatusPendingApproval {
		t.Fatalf("a plan of a clean module was held: %q", got.DryRunFindings())
	}
	if n := reg.downloadsOf("clean"); n != 1 {
		t.Errorf("the module was downloaded %d times for one probe and one submission, want 1", n)
	}
	// A change to the configuration is a different configuration, so it is downloaded and read
	// again rather than answered from what was read before.
	dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": reg.call("clean") + "\n# edited\n"})
	if _, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if n := reg.downloadsOf("clean"); n != 2 {
		t.Errorf("the edited configuration was not downloaded again: %d downloads, want 2", n)
	}
}

// TestTheGateDownloadsOnlyWhereThePlanRuns covers the download's reach. It runs with the network
// the plan would have, so a plan routed to a worker is not downloaded for here, and a runner that
// cannot download leaves the module unread as before. Each says why in the record.
func TestTheGateDownloadsOnlyWhereThePlanRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
	tests := []struct {
		// CanFetch gives the runner a download.
		CanFetch bool
		// Queue is the queue the plan is routed to.
		Queue string
		// WantReason is why the module stays unread.
		WantReason string
	}{{ // Test 0: A plan routed to a worker's queue.
		CanFetch: true, Queue: "far",
		WantReason: `not downloaded, since the plan runs on a worker for queue "far"`,
	}, { // Test 1: A runner with no download.
		WantReason: "not downloaded, since this server's runner cannot download modules",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			get := &stubGet{}
			runner := roundhouse.Runner(dispatchtest.OKRunner())
			if test.CanFetch {
				runner = fetchingRunner{Runner: dispatchtest.OKRunner(), fetch: get}
			}
			d := New(run.NewMemStore(), runner, zap.NewNop(), WithNoJanitor())
			t.Cleanup(d.Close)
			scan := d.scanPlan(&run.Run{ID: "run_far", Tool: run.ToolTerraform, Command: dir,
				DryRun: true, Queue: test.Queue}, nil)
			if scan.Fetch != nil || scan.Classification != run.DryRunIncomplete ||
				!strings.Contains(strings.Join(scan.Unread, "\n"), test.WantReason) {
				t.Errorf("scan = %+v, want incomplete with no download, saying %q", scan,
					test.WantReason)
			}
			if n := len(get.handed()); n != 0 {
				t.Errorf("the gate downloaded %d times for a plan it does not run", n)
			}
		})
	}
}

// stubModule is one module a stubGet installs.
type stubModule struct {
	// Key is the module's key in the manifest, its call's name.
	Key string
	// Source is the source the manifest records.
	Source string
	// Version is the version the manifest records, empty for a source with none.
	Version string
	// Files are the module's files by path.
	Files map[string]string
}

// stubGet stands in for the tool's get. It records every spec it is handed and installs its
// modules where a get leaves them, a manifest and each module's files under .terraform/modules,
// then reports exit.
type stubGet struct {
	// modules are the modules it installs.
	modules []stubModule
	// exit is the exit status it reports.
	exit int
	// mu guards specs.
	mu sync.Mutex
	// specs are the specs it was handed, in order.
	specs []roundhouse.Spec
}

// FetchModules records spec and installs the modules into spec's working directory.
func (s *stubGet) FetchModules(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	modules := filepath.Join(spec.Dir, filepath.FromSlash(spec.Command), ".terraform", "modules")
	manifest := `{"Modules":[{"Key":"","Source":"","Dir":"."}`
	for _, m := range s.modules {
		manifest += fmt.Sprintf(`,{"Key":%q,"Source":%q,"Version":%q,"Dir":%q}`, m.Key, m.Source,
			m.Version, ".terraform/modules/"+m.Key)
		for name, body := range m.Files {
			path := filepath.Join(modules, m.Key, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return roundhouse.Result{ExitCode: -1}, err
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return roundhouse.Result{ExitCode: -1}, err
			}
		}
	}
	if err := os.MkdirAll(modules, 0o700); err != nil {
		return roundhouse.Result{ExitCode: -1}, err
	}
	if err := os.WriteFile(filepath.Join(modules, "modules.json"), []byte(manifest+"]}"),
		0o600); err != nil {
		return roundhouse.Result{ExitCode: -1}, err
	}
	return roundhouse.Result{ExitCode: s.exit}, nil
}

// handed returns the specs the get was handed.
func (s *stubGet) handed() []roundhouse.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.specs)
}

// countingSource is the kind of the secret source that counts every credential it opens.
const countingSource = "test_gate_counting"

// gateMints counts what countingSource opened, by the configuration each credential sealed, so
// parallel cases each read their own count.
var gateMints = struct {
	// mu guards counts.
	mu sync.Mutex
	// counts holds each configuration's openings.
	counts map[string]int
}{counts: map[string]int{}}

// init registers countingSource once, before any test reads the source registry.
func init() {
	secretsource.RegisterDynamic(countingSource, func(_ context.Context, config string) (string,
		*secretsource.Lease, error) {
		gateMints.mu.Lock()
		gateMints.counts[config]++
		gateMints.mu.Unlock()
		return "GATE_SECRET=gate-secret-" + config, secretsource.NewLease(countingSource,
			func(context.Context) error { return nil }), nil
	})
}

// mintsOf returns how many times countingSource opened the credential that sealed config.
func mintsOf(config string) int {
	gateMints.mu.Lock()
	defer gateMints.mu.Unlock()
	return gateMints.counts[config]
}

// TestTheGateDownloadsWithNothingOfTheRuns covers what the gate's download is handed and for whom
// it runs at all. An agent's plan is held before anything is fetched for it: neither its counting
// credential nor the get is reached. A person's plan, and a pull request review's pre-check, reach
// the get with no image, no credential file, no credential value, and none of the run's variables,
// on this server's binary started from the minimal environment, and the credential they name is
// never opened. A plan that runs in an image is not downloaded at all.
//
//nolint:funlen // Test function.
func TestTheGateDownloadsWithNothingOfTheRuns(t *testing.T) {
	t.Parallel()
	image := "registry.example.test/tf@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		// Opts say who submits and with what.
		Opts []run.SubmitOption
		// Precheck asks through DryRunScansAt, as a pull request review does, instead of submitting.
		Precheck bool
		// WantHeld is whether the submission is held for a person.
		WantHeld bool
		// WantFetches is how many times the get is reached.
		WantFetches int
		// WantReason is a fragment of why the module stays unread, empty when it is downloaded.
		WantReason string
	}{{ // Test 0: An agent's plan is held, and nothing is opened or fetched for it.
		Opts: dispatchtest.AgentOpts(), WantHeld: true, WantReason: agentNotDownloaded,
	}, { // Test 1: A person's plan is downloaded with nothing of the run's.
		Opts: dispatchtest.PersonOpts(), WantFetches: 1,
	}, { // Test 2: A review's pre-check is downloaded with nothing of the template's.
		Precheck: true, WantFetches: 1,
	}, { // Test 3: A plan that names the container image it runs in is not downloaded.
		Opts:       append(dispatchtest.PersonOpts(), run.WithImage(image, "")),
		WantReason: imageNotDownloaded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			config := fmt.Sprintf("%s-%d", t.Name(), testNum)
			sealer := credential.NewSealer("pass", "salt")
			creds := credential.NewMemStore()
			for _, c := range []struct {
				// ID is the credential's id.
				ID string
				// Kind is its kind.
				Kind credential.Kind
				// Source is where its secret comes from, empty for the stored value.
				Source string
				// Secret is what is sealed: the source's configuration, or the value itself.
				Secret string
			}{
				{ID: "cred_count", Kind: credential.KindEnv, Source: countingSource, Secret: config},
				{ID: "cred_kc", Kind: credential.KindKubeconfig, Secret: fileKubeconfig},
			} {
				sealed, err := sealer.Seal(c.Secret)
				if err != nil {
					t.Fatalf("Seal() error = %v", err)
				}
				if err := creds.Save(ctx, &credential.Credential{ID: c.ID, Name: c.ID, Kind: c.Kind,
					Source: c.Source, Secret: sealed}); err != nil {
					t.Fatalf("Save(credential) error = %v", err)
				}
			}
			dir := t.TempDir()
			dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
			get := &stubGet{}
			d := New(run.NewMemStore(), fetchingRunner{Runner: dispatchtest.OKRunner(), fetch: get},
				zap.NewNop(), WithCredentials(creds, sealer), WithRunFilesRoot(t.TempDir()),
				WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
				WithNoJanitor())
			t.Cleanup(d.Close)
			named := []string{"cred_count", "cred_kc"}
			var scans []run.DryRunScan
			if test.Precheck {
				scans = d.DryRunScansAt(ctx, &run.Run{Tool: run.ToolTerraform, Command: dir,
					DryRun: true, CredentialIDs: named, ExtraVars: map[string]any{"region": "x"},
					Labels: map[string]string{run.LabelPullRequest: "7"}})
			} else {
				got, err := d.Submit(ctx, "", "", append([]run.SubmitOption{
					run.WithTool(run.ToolTerraform), run.WithCommand(dir), run.WithDryRun(true),
					run.WithCredentialIDs(named),
					run.WithExtraVars(map[string]any{"region": "x"})}, test.Opts...)...)
				if err != nil {
					t.Fatalf("Submit() error = %v", err)
				}
				if test.WantHeld && got.Status != run.StatusPendingApproval {
					t.Errorf("status = %q, want held for a person", got.Status)
				}
				scans = got.DryRunScans
			}
			if n := mintsOf(config); n != 0 {
				t.Errorf("the run's credential was opened %d times before anyone approved it", n)
			}
			handed := get.handed()
			if len(handed) != test.WantFetches {
				t.Fatalf("the get was reached %d times, want %d", len(handed), test.WantFetches)
			}
			if len(scans) != 1 {
				t.Fatalf("scans = %+v, want the gate's one scan", scans)
			}
			if test.WantReason != "" && !strings.Contains(strings.Join(scans[0].Unread, "\n"),
				test.WantReason) {
				t.Errorf("unread = %q, want the module unread saying %q", scans[0].Unread,
					test.WantReason)
			}
			for _, spec := range handed {
				if spec.Image != "" || len(spec.CredentialFiles) != 0 || len(spec.ExtraVars) != 0 ||
					spec.RegistryUsername != "" || spec.RegistryPassword != "" {
					t.Errorf("the get was handed image %q, files %q, variables %v, and a pull "+
						"login %q, want none of them", spec.Image, spec.CredentialFiles,
						spec.ExtraVars, spec.RegistryUsername)
				}
				for _, e := range spec.Env {
					if strings.Contains(e, "GATE_SECRET") || strings.Contains(e, "KUBECONFIG") ||
						strings.Contains(e, fileSecret) {
						t.Errorf("the get was handed %q, which only the run executes with", e)
					}
				}
				if !spec.MinimalEnv || spec.RunDir == "" {
					t.Errorf("minimal environment %v in %q, want the get started from the "+
						"minimal environment with a private home", spec.MinimalEnv, spec.RunDir)
				}
			}
		})
	}
}

// unreadableProjects is a project store whose Get fails, standing for a project the gate cannot
// read at the moment it asks.
type unreadableProjects struct {
	// Store answers every other call.
	project.Store
}

// Get fails.
func (unreadableProjects) Get(context.Context, string) (*project.Project, error) {
	return nil, errors.New("the project store did not answer")
}

// TestAPlanInAContainerImageIsNotDownloaded covers a plan that runs in a container image it did
// not name itself. The gate starts no image, and the host's get is not the image's, so a plan whose
// project names an image, or that takes the server's default image, is not downloaded and stays
// unclassified with why. A pull request's pre-check, which is never pinned to an image, gets the
// gate's answer, since both resolve the image the same way. A project that cannot be read to learn
// whether it names one is not downloaded either. A plan with no image anywhere is the control, and
// is downloaded.
//
//nolint:funlen // Test function.
func TestAPlanInAContainerImageIsNotDownloaded(t *testing.T) {
	t.Parallel()
	image := "registry.example.test/tf@sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		// ProjectImage is the image the project names, empty for none.
		ProjectImage string
		// DefaultImage is the server's default image, empty for none.
		DefaultImage string
		// Unreadable makes the project unreadable when the gate asks.
		Unreadable bool
		// Precheck asks through DryRunScansAt, as a pull request review does, instead of submitting.
		Precheck bool
		// WantFetches is how many times the get is reached.
		WantFetches int
		// WantReason is a fragment of why the module stays unread, empty when it is downloaded.
		WantReason string
	}{{ // Test 0: A project naming an image, at the gate.
		ProjectImage: image, WantReason: imageNotDownloaded,
	}, { // Test 1: The same, at a pull request's pre-check.
		ProjectImage: image, Precheck: true, WantReason: imageNotDownloaded,
	}, { // Test 2: The server's default image under a project naming none, at the gate.
		DefaultImage: image, WantReason: imageNotDownloaded,
	}, { // Test 3: The same, at a pull request's pre-check.
		DefaultImage: image, Precheck: true, WantReason: imageNotDownloaded,
	}, { // Test 4: A project that cannot be read to say whether it names an image.
		Unreadable: true, Precheck: true, WantReason: imageUnknownNotDownloaded,
	}, { // Test 5: No image anywhere is downloaded at the gate, the control.
		WantFetches: 1,
	}, { // Test 6: And at a pull request's pre-check.
		Precheck: true, WantFetches: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo := newTreeRepo(t, map[string]string{"infra/main.tf": registryCall("clean")})
			syncer, err := project.NewSyncer(t.TempDir())
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			p := &project.Project{ID: "proj_image", Name: "infra", RepoURL: repo, Branch: "main",
				Image: test.ProjectImage}
			var projects project.Store = project.NewMemStore()
			if err := projects.Save(ctx, p); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			if test.Unreadable {
				projects = unreadableProjects{Store: projects}
			}
			wt, err := syncer.Sync(p, "")
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			wt.Cleanup()
			get := &stubGet{}
			d := New(run.NewMemStore(), fetchingRunner{Runner: dispatchtest.OKRunner(), fetch: get},
				zap.NewNop(), WithProjects(projects, syncer), WithDefaultImage(test.DefaultImage),
				WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
				WithRunFilesRoot(t.TempDir()), WithNoJanitor())
			t.Cleanup(d.Close)
			var scans []run.DryRunScan
			if test.Precheck {
				scans = d.DryRunScansAt(ctx, &run.Run{Tool: run.ToolTerraform, Command: "infra",
					ProjectID: p.ID, DryRun: true})
			} else {
				got, err := d.Submit(ctx, "", "", append([]run.SubmitOption{
					run.WithTool(run.ToolTerraform), run.WithCommand("infra"), run.WithProject(p.ID),
					run.WithDryRun(true)}, dispatchtest.PersonOpts()...)...)
				if err != nil {
					t.Fatalf("Submit() error = %v", err)
				}
				scans = got.DryRunScans
			}
			if n := len(get.handed()); n != test.WantFetches {
				t.Errorf("the get was reached %d times, want %d", n, test.WantFetches)
			}
			if len(scans) != 1 {
				t.Fatalf("scans = %+v, want the gate's one scan", scans)
			}
			unread := strings.Join(scans[0].Unread, "\n")
			if test.WantReason != "" && (scans[0].Classification != run.DryRunIncomplete ||
				!strings.Contains(unread, test.WantReason)) {
				t.Errorf("scan %q with unread %q, want unclassified saying %q",
					scans[0].Classification, unread, test.WantReason)
			}
			if strings.Contains(unread, image) {
				t.Errorf("unread %q names the image, which a refusal comment would repeat", unread)
			}
		})
	}
}

// TestTheGateReadsRegistryModulesItsGetInstalled covers the registry cases without a registry. A
// stand-in get installs what the real get leaves for a registry module, a manifest naming the
// source and version and the module's files, and the gate reads it the way it reads a real get's:
// a clean module passes, a hidden external data source is caught by address, a short address is
// matched to the host it normalizes to, a version the constraint does not allow is left unread,
// and a get that fails leaves the module unread with why.
//
//nolint:funlen // Test function.
func TestTheGateReadsRegistryModulesItsGetInstalled(t *testing.T) {
	t.Parallel()
	clean := map[string]string{"main.tf": "output \"o\" {\n  value = 1\n}\n"}
	tests := []struct {
		// Source is the module source the configuration names.
		Source string
		// Installed is the module the get installs.
		Installed stubModule
		// Exit is the get's exit status.
		Exit int
		// WantClass is the scan's classification.
		WantClass string
		// WantEntry is a fragment of a finding or an unread entry, empty for none.
		WantEntry string
	}{{ // Test 0: A clean registry module passes, pinned to what was read.
		Source: registryHost + "/acme/clean/null",
		Installed: stubModule{Key: "net", Source: registryHost + "/acme/clean/null",
			Version: "1.0.0", Files: clean},
		WantClass: run.DryRunChangeFree,
	}, { // Test 1: One hiding an external data source behind a nested module is caught.
		Source: registryHost + "/acme/hiding/null",
		Installed: stubModule{Key: "net", Source: registryHost + "/acme/hiding/null",
			Version: "1.0.0", Files: testModules()["hiding"].Files},
		WantClass: run.DryRunNotChangeFree,
		WantEntry: "module.net.module.inner.data.external.x runs a program during plan " +
			"(.terraform/modules/net/nested/main.tf line 1)",
	}, { // Test 2: A short address is read as the default registry's.
		Source: "acme/clean/null",
		Installed: stubModule{Key: "net", Source: "registry.terraform.io/acme/clean/null",
			Version: "1.0.0", Files: clean},
		WantClass: run.DryRunChangeFree,
	}, { // Test 3: A version the constraint does not allow is not read.
		Source: registryHost + "/acme/clean/null",
		Installed: stubModule{Key: "net", Source: registryHost + "/acme/clean/null",
			Version: "2.0.0", Files: clean},
		WantClass: run.DryRunIncomplete,
		WantEntry: `version "2.0.0", which "~> 1.0" does not allow`,
	}, { // Test 4: A get that fails leaves the module unread with why.
		Source:    registryHost + "/acme/clean/null",
		Exit:      1,
		WantClass: run.DryRunIncomplete,
		WantEntry: "not downloaded, since the gate's terraform get failed with exit status 1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			dispatchtest.WriteFiles(t, dir, map[string]string{"main.tf": fmt.Sprintf(
				"module \"net\" {\n  source  = %q\n  version = \"~> 1.0\"\n}\n", test.Source)})
			get := &stubGet{exit: test.Exit}
			if test.Installed.Key != "" {
				get.modules = []stubModule{test.Installed}
			}
			d := New(run.NewMemStore(), fetchingRunner{Runner: dispatchtest.OKRunner(), fetch: get},
				zap.NewNop(), WithRunFilesRoot(t.TempDir()), WithNoJanitor())
			t.Cleanup(d.Close)
			scan := d.scanPlan(&run.Run{ID: "run_registry", Tool: run.ToolTerraform, Command: dir,
				DryRun: true}, nil)
			if scan.Classification != test.WantClass {
				t.Errorf("classification = %q, want %q (findings %q, unread %q, fetch %+v)",
					scan.Classification, test.WantClass, scan.Findings, scan.Unread, scan.Fetch)
			}
			if test.WantEntry != "" && !strings.Contains(strings.Join(append(scan.Findings,
				scan.Unread...), "\n"), test.WantEntry) {
				t.Errorf("findings %q and unread %q, want one saying %q", scan.Findings, scan.Unread,
					test.WantEntry)
			}
			if test.WantClass == run.DryRunChangeFree && (scan.Fetch == nil ||
				!strings.HasPrefix(scan.Fetch.ModulesDigest, "sha256:")) {
				t.Errorf("fetch = %+v, want the modules the gate read pinned", scan.Fetch)
			}
			if n := len(get.handed()); n != 1 {
				t.Errorf("the get was reached %d times, want 1", n)
			}
		})
	}
}

// TestAProjectPlanDownloadsItsModulesAtTheCommit covers a plan drawn from a project, which is how a
// pull request review and a template plan. The gate copies what it read at the commit, downloads
// the modules into the copy, and reads them, so a module pushed with a hidden external data source
// is caught at the commit about to run.
func TestAProjectPlanDownloadsItsModulesAtTheCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, testModules())
	repo := newTreeRepo(t, map[string]string{"infra/main.tf": reg.call("clean")})
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_mods", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(), WithProjects(projects, syncer),
		WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
		WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	wt, err := syncer.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	submit := func() *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand("infra"),
			run.WithProject(p.ID), run.WithDryRun(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return r
	}
	clean := submit()
	if clean.Status == run.StatusPendingApproval || clean.DryRunScans[0].Fetch == nil {
		t.Fatalf("a clean project plan = %q with scans %+v, want it downloaded and passed",
			clean.Status, clean.DryRunScans)
	}
	if !strings.Contains(clean.DryRunScans[0].Source, "read at commit ") {
		t.Errorf("source = %q, want the commit the gate read", clean.DryRunScans[0].Source)
	}
	commitTree(t, repo, map[string]string{"infra/main.tf": reg.call("hiding")})
	held := submit()
	want := "module.net.module.inner.data.external.x runs a program during plan " +
		"(infra/.terraform/modules/net/nested/main.tf line 1)"
	if held.Status != run.StatusPendingApproval || !slices.Contains(held.DryRunFindings(), want) {
		t.Errorf("a project plan calling a module hiding an external data source = %q with %q, "+
			"want held naming %q", held.Status, held.DryRunFindings(), want)
	}
}

// TestTheGateIgnoresACommittedManifestAndDownloadsTheRealModule is the end-to-end control for the
// forged-manifest bypass: a plan whose tree carries a .terraform the author planted, a module
// manifest pointing a remote module at a clean copy committed beside it, must not read as change
// free. The gate ignores the committed .terraform, downloads the real module, which hides an
// external data source, and holds the plan. Without distrusting the committed copy the gate read it
// and let the plan through, so the real module's program ran during plan.
func TestTheGateIgnoresACommittedManifestAndDownloadsTheRealModule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, testModules())
	source := reg.srv.URL + "/archive/hiding-1.0.0.tar.gz"
	dir := t.TempDir()
	// The committed tree: a call to the module "hiding", plus a planted clean copy and a manifest
	// that names it, the shape a pull request author commits to dodge the gate.
	dispatchtest.WriteFiles(t, dir, map[string]string{
		"main.tf": reg.call("hiding"),
		".terraform/modules/modules.json": "{\"Modules\":[{\"Key\":\"\",\"Source\":\"\",\"Dir\":\".\"}," +
			"{\"Key\":\"net\",\"Source\":\"" + source + "\",\"Dir\":\".terraform/modules/net\"}]}",
		".terraform/modules/net/main.tf": "output \"o\" {\n  value = 1\n}\n",
	})
	files := t.TempDir()
	// The download gets a minute, so a busy machine never stops it before it reads the module.
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(),
		WithPolicies(dispatchtest.RulesHolding(t, excludePlans(run.ToolTerraform))),
		WithRunFilesRoot(files), WithNoJanitor(), WithModuleFetchLimits(time.Minute, 1<<20))
	t.Cleanup(d.Close)

	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if len(got.DryRunScans) != 1 {
		t.Fatalf("scans = %+v, want the gate's one scan", got.DryRunScans)
	}
	scan := got.DryRunScans[0]
	want := "module.net.module.inner.data.external.x runs a program during plan " +
		"(.terraform/modules/net/nested/main.tf line 1)"
	if scan.Classification != run.DryRunNotChangeFree || !slices.Contains(scan.Findings, want) {
		t.Errorf("classification = %q findings %q, want not_change_free naming %q (the gate must "+
			"download the real module, not trust the committed copy); fetch %+v",
			scan.Classification, scan.Findings, want, scan.Fetch)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %q, want held", got.Status)
	}
}
