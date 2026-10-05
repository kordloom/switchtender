package dispatch

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
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
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// registryHost is the registry host the test configurations name. It resolves nowhere: the CLI
// configuration the run's credential carries points it at the test registry.
const registryHost = "registry.example.test"

// registryModule is one module the test registry serves, as acme/<name>/null version 1.0.0.
type registryModule struct {
	// Files are the module's files by path.
	Files map[string]string
	// Hang makes its download never answer, until the client gives up.
	Hang bool
	// Pad adds a file of this many bytes to its archive.
	Pad int
}

// testRegistry serves the Terraform module registry protocol for the modules it holds, each
// downloaded as a tar.gz archive from the same server. Every module has version 1.0.0, and a test
// can publish 1.1.0 of one while the test runs.
type testRegistry struct {
	// srv is the HTTP server.
	srv *httptest.Server
	// modules are the modules served, by name.
	modules map[string]registryModule
	// mu guards downloads and newer.
	mu sync.Mutex
	// downloads counts each module's archive downloads.
	downloads map[string]int
	// newer holds the files of each module's published 1.1.0, by name.
	newer map[string]map[string]string
}

// newTestRegistry starts a registry serving modules and stops it when the test ends.
func newTestRegistry(t *testing.T, modules map[string]registryModule) *testRegistry {
	t.Helper()
	reg := &testRegistry{modules: modules, downloads: map[string]int{},
		newer: map[string]map[string]string{}}
	reg.srv = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.srv.Close)
	return reg
}

// publish makes version 1.1.0 of the named module available, holding files.
func (reg *testRegistry) publish(name string, files map[string]string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.newer[name] = files
}

// serve answers the registry's version listing, its download redirect, and the archive itself.
func (reg *testRegistry) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 6 && parts[0] == "v1" && parts[5] == "versions":
		if _, ok := reg.modules[parts[3]]; !ok {
			http.NotFound(w, r)
			return
		}
		reg.mu.Lock()
		_, published := reg.newer[parts[3]]
		reg.mu.Unlock()
		if published {
			_, _ = io.WriteString(w, `{"modules":[{"versions":[{"version":"1.0.0"},`+
				`{"version":"1.1.0"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"modules":[{"versions":[{"version":"1.0.0"}]}]}`)
	case len(parts) == 7 && parts[0] == "v1" && parts[6] == "download":
		w.Header().Set("X-Terraform-Get", reg.srv.URL+"/archive/"+parts[3]+"-"+parts[5]+".tar.gz")
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[0] == "archive":
		name, version, _ := strings.Cut(strings.TrimSuffix(parts[1], ".tar.gz"), "-")
		mod, ok := reg.modules[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reg.mu.Lock()
		if files, published := reg.newer[name]; published && version == "1.1.0" {
			mod = registryModule{Files: files}
		}
		reg.mu.Unlock()
		archive := moduleArchive(mod)
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

// downloadsOf returns how many times a module's archive was downloaded.
func (reg *testRegistry) downloadsOf(name string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.downloads[name]
}

// cliConfig writes a CLI configuration that points registryHost at the registry and returns its
// path.
func (reg *testRegistry) cliConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.tfrc")
	body := fmt.Sprintf("host %q {\n  services = {\n    \"modules.v1\" = %q\n  }\n}\n", registryHost,
		reg.srv.URL+"/v1/modules/")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write CLI configuration: %v", err)
	}
	return path
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
	return fetchingRunner{Runner: okRunner(), fetch: fetch}
}

// cliCredential stores an env credential carrying the CLI configuration path, which is how a run
// reaches a private registry, and returns the credential store and its sealer.
func cliCredential(t *testing.T, cliConfig string) (credential.Store, *credential.Sealer) {
	t.Helper()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("TF_CLI_CONFIG_FILE=" + cliConfig)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{ID: "cred_tf",
		Name: "terraform cli", Kind: credential.KindEnv, Secret: sealed}); err != nil {
		t.Fatalf("Save(credential) error = %v", err)
	}
	return creds, sealer
}

// registryCall is a configuration calling the named module from the test registry.
func registryCall(name string) string {
	return fmt.Sprintf("module \"net\" {\n  source  = \"%s/acme/%s/null\"\n"+
		"  version = \"~> 1.0\"\n}\n", registryHost, name)
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
		"loops": {Files: map[string]string{"main.tf": registryCall("loops")}},
	}
}

// TestTheGateDownloadsModulesBeforeItReadsAPlan drives the gate's module download through the
// dispatcher, with real terraform against a local registry.
//
// A fresh commit holds no downloaded modules, so a plan calling a registry module was held or
// refused whatever the module held. The gate now downloads the modules first, with the run's own
// credentials, here the CLI configuration that reaches the registry, and reads what was installed.
// A clean module now passes, one hiding an external data source is still caught by address, and a
// download that fails, hangs, grows too large, or loops leaves the plan unclassified with why.
//
//nolint:funlen // Test function.
func TestTheGateDownloadsModulesBeforeItReadsAPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Module is the registry module the configuration calls.
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
	}{{ // Test 0: A clean registry module now passes.
		Module: "clean", WantClass: run.DryRunChangeFree, WantFetched: true,
	}, { // Test 1: One hiding an external data source behind a nested module is still caught.
		Module: "hiding", WantClass: run.DryRunNotChangeFree, WantFetched: true,
		WantFinding: "module.net.module.inner.data.external.x runs a program during plan " +
			"(.terraform/modules/net/nested/main.tf line 1)",
	}, { // Test 2: A module the registry does not have fails the download, which fails closed.
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
			creds, sealer := cliCredential(t, reg.cliConfig(t))
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": registryCall(test.Module)})
			store := run.NewMemStore()
			files := t.TempDir()
			d := New(store, newFetchingRunner(t), zap.NewNop(),
				WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
				WithCredentials(creds, sealer), WithRunFilesRoot(files), WithNoJanitor(),
				WithModuleFetchLimits(cmp.Or(test.Timeout, time.Minute), 1<<20))
			t.Cleanup(d.Close)

			got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
				run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"}))
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
	creds, sealer := cliCredential(t, reg.cliConfig(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
		WithCredentials(creds, sealer), WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	probe := &run.Run{Tool: run.ToolTerraform, Command: dir, DryRun: true,
		CredentialIDs: []string{"cred_tf"}}
	if scans := d.DryRunScansAt(ctx, probe); !run.ScansChangeFree(scans) {
		t.Fatalf("the probe's scan = %+v, want change free", scans)
	}
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.Status == run.StatusPendingApproval {
		t.Fatalf("a plan of a clean registry module was held: %q", got.DryRunFindings())
	}
	if n := reg.downloadsOf("clean"); n != 1 {
		t.Errorf("the module was downloaded %d times for one probe and one submission, want 1", n)
	}
	// A change to the configuration is a different configuration, so it is downloaded and read
	// again rather than answered from what was read before.
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean") + "\n# edited\n"})
	if _, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"})); err != nil {
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
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
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
			var mu sync.Mutex
			fetches := 0
			runner := roundhouse.Runner(okRunner())
			if test.CanFetch {
				runner = fetchingRunner{Runner: okRunner(), fetch: roundhouse.ModuleFetcherFunc(
					func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
						mu.Lock()
						fetches++
						mu.Unlock()
						return roundhouse.Result{ExitCode: 0}, nil
					})}
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
			mu.Lock()
			defer mu.Unlock()
			if fetches != 0 {
				t.Errorf("the gate downloaded %d times for a plan it does not run", fetches)
			}
		})
	}
}

// TestTheGateDownloadsWithTheRunsCredentialsAlone covers what the download is handed: the
// credentials the run itself executes with, opened the way execution opens them, and nothing
// more. A credential the run does not name stays out, as do the run's variables, and a download
// that fails is recorded with every secret it was handed masked and the credentials a URL carries
// removed.
//
//nolint:funlen // Test function.
func TestTheGateDownloadsWithTheRunsCredentialsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	creds := credential.NewMemStore()
	for id, value := range map[string]string{
		"cred_tf":    "TF_CLI_CONFIG_FILE=/etc/cli.tfrc\nREGISTRY_TOKEN=registry-token-2468",
		"cred_other": "OTHER_SECRET=other-secret-1357",
	} {
		sealed, err := sealer.Seal(value)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		if err := creds.Save(ctx, &credential.Credential{ID: id, Name: id, Kind: credential.KindEnv,
			Secret: sealed}); err != nil {
			t.Fatalf("Save(credential) error = %v", err)
		}
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
	tests := []struct {
		// Creds are the credentials the run names.
		Creds []string
		// WantEnv are entries the download must be handed.
		WantEnv []string
		// WantNotEnv are prefixes of entries it must not be handed.
		WantNotEnv []string
	}{{ // Test 0: The run's own credential reaches the download, and no other.
		Creds:      []string{"cred_tf"},
		WantEnv:    []string{"TF_CLI_CONFIG_FILE=/etc/cli.tfrc", "REGISTRY_TOKEN=registry-token-2468"},
		WantNotEnv: []string{"OTHER_SECRET=", "TF_VAR_"},
	}, { // Test 1: A run naming no credential hands the download none.
		WantNotEnv: []string{"TF_CLI_CONFIG_FILE=", "REGISTRY_TOKEN=", "OTHER_SECRET=", "TF_VAR_"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var handed roundhouse.Spec
			// The download fails the way a registry refusing a token does, and echoes what it was
			// handed, as a careless error line would.
			runner := fetchingRunner{Runner: okRunner(), fetch: roundhouse.ModuleFetcherFunc(
				func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					mu.Lock()
					handed = spec
					mu.Unlock()
					token := ""
					for _, e := range spec.Env {
						if v, ok := strings.CutPrefix(e, "REGISTRY_TOKEN="); ok {
							token = v
						}
					}
					_, _ = fmt.Fprintf(out, "Error: could not reach https://ci:%s@%s/ with %s\n",
						token, registryHost, token)
					return roundhouse.Result{ExitCode: 1}, nil
				})}
			d := New(run.NewMemStore(), runner, zap.NewNop(), WithCredentials(creds, sealer),
				WithRunFilesRoot(t.TempDir()), WithNoJanitor())
			t.Cleanup(d.Close)
			scan := d.scanPlan(&run.Run{ID: "run_creds", Tool: run.ToolTerraform, Command: dir,
				DryRun: true, CredentialIDs: test.Creds,
				ExtraVars: map[string]any{"region": "us-east-1"}}, nil)
			mu.Lock()
			defer mu.Unlock()
			for _, want := range test.WantEnv {
				if !slices.Contains(handed.Env, want) {
					t.Errorf("the download was not handed %q: %q", want, handed.Env)
				}
			}
			for _, e := range handed.Env {
				for _, not := range test.WantNotEnv {
					if strings.HasPrefix(e, not) {
						t.Errorf("the download was handed %q, which the run does not execute with", e)
					}
				}
			}
			if len(handed.ExtraVars) != 0 {
				t.Errorf("the download was handed the run's variables: %v", handed.ExtraVars)
			}
			if scan.Fetch == nil || scan.Fetch.ExitStatus != 1 ||
				scan.Classification != run.DryRunIncomplete {
				t.Fatalf("scan = %+v, want the failed download recorded and the plan unclassified",
					scan)
			}
			if strings.Contains(scan.Fetch.Error, "registry-token-2468") ||
				strings.Contains(scan.Fetch.Error, "ci:") {
				t.Errorf("the record repeats a secret: %q", scan.Fetch.Error)
			}
		})
	}
}

// TestAProjectPlanDownloadsItsModulesAtTheCommit covers a plan drawn from a project, which is how a
// pull request review and a template plan. The gate copies what it read at the commit, downloads
// the modules into the copy, and reads them, so a registry module pushed with a hidden external
// data source is caught at the commit about to run.
func TestAProjectPlanDownloadsItsModulesAtTheCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, testModules())
	creds, sealer := cliCredential(t, reg.cliConfig(t))
	repo := newTreeRepo(t, map[string]string{"infra/main.tf": registryCall("clean")})
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
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
		WithCredentials(creds, sealer), WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	wt, err := syncer.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	submit := func() *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand("infra"),
			run.WithProject(p.ID), run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"}))
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
	commitTree(t, repo, map[string]string{"infra/main.tf": registryCall("hiding")})
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
// manifest pointing a registry module at a clean copy committed beside it, must not read as change
// free. The gate ignores the committed .terraform, downloads the real module, which hides an
// external data source, and holds the plan. Without distrusting the committed copy the gate read it
// and let the plan through, so the real module's program ran during plan with the run's credentials.
func TestTheGateIgnoresACommittedManifestAndDownloadsTheRealModule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, testModules())
	creds, sealer := cliCredential(t, reg.cliConfig(t))
	dir := t.TempDir()
	// The committed tree: a call to the registry module "hiding", plus a planted clean copy and a
	// manifest that names it, the shape a pull request author commits to dodge the gate.
	writeFiles(t, dir, map[string]string{
		"main.tf": registryCall("hiding"),
		".terraform/modules/modules.json": "{\"Modules\":[{\"Key\":\"\",\"Source\":\"\",\"Dir\":\".\"}," +
			"{\"Key\":\"net\",\"Source\":\"" + registryHost + "/acme/hiding/null\"," +
			"\"Version\":\"1.0.0\",\"Dir\":\".terraform/modules/net\"}]}",
		".terraform/modules/net/main.tf": "output \"o\" {\n  value = 1\n}\n",
	})
	files := t.TempDir()
	// The download gets a minute, so a busy machine never stops it before it reads the module.
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
		WithCredentials(creds, sealer), WithRunFilesRoot(files), WithNoJanitor(),
		WithModuleFetchLimits(time.Minute, 1<<20))
	t.Cleanup(d.Close)

	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"}))
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
