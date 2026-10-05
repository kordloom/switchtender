package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// fetchModuleRepo builds a git repository holding a module, tagged v1.0.0, and returns its path.
func fetchModuleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("output \"o\" {\n  value = 1\n}\n"),
		0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=test@example.invalid", "-c", "user.name=test", "add", "-A"},
		{"-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-q", "-m", "m"},
		{"tag", "v1.0.0"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// TestFetchModulesDownloadsModulesAndNothingElse holds the gate's module download to what it
// promises, with the real tools: the modules a configuration calls are installed where the tool
// keeps them, and nothing else happens. No provider is installed, though the configuration requires
// one, and the program an external data source names does not run, though a plan would run it.
//
//nolint:funlen // Test function.
func TestFetchModulesDownloadsModulesAndNothingElse(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		standDown(t, "git is not on PATH, so no module can be installed from a repository")
	}
	tests := []struct {
		// Tool is the tool fetched for.
		Tool string
		// Binary is the executable that tool runs.
		Binary string
	}{{ // Test 0: Terraform.
		Tool: run.ToolTerraform, Binary: "terraform",
	}, { // Test 1: OpenTofu.
		Tool: run.ToolOpenTofu, Binary: "tofu",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(test.Binary); err != nil {
				standDown(t, "%s is not on PATH, so the real get cannot run", test.Binary)
			}
			repo := fetchModuleRepo(t)
			work := t.TempDir()
			marker := filepath.Join(work, "ran")
			config := fmt.Sprintf("terraform {\n  required_providers {\n    external = {\n"+
				"      source = \"hashicorp/external\"\n    }\n  }\n}\n"+
				"module \"net\" {\n  source = \"git::file://%s?ref=v1.0.0\"\n}\n"+
				"data \"external\" \"probe\" {\n  program = [\"sh\", \"-c\", \"touch %s; echo {}\"]\n}\n",
				filepath.ToSlash(repo), filepath.ToSlash(marker))
			if err := os.WriteFile(filepath.Join(work, "main.tf"), []byte(config), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			fetcher, ok := NewAnsibleRunner().(ModuleFetcher)
			if !ok {
				t.Fatal("the runner cannot fetch modules")
			}
			var out bytes.Buffer
			// The tool's version check would call home, and this test reaches nothing remote.
			res, err := fetcher.FetchModules(context.Background(), Spec{Tool: test.Tool, Command: ".",
				Dir: work, DryRun: true, Env: []string{"CHECKPOINT_DISABLE=1"}}, &out)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("FetchModules() = %d, %v\n%s", res.ExitCode, err, out.String())
			}
			manifest, err := os.ReadFile(filepath.Join(work, ".terraform", "modules", "modules.json"))
			if err != nil || !strings.Contains(string(manifest), `"Key":"net"`) {
				t.Errorf("modules.json = %s, %v, want the module recorded", manifest, err)
			}
			installed := filepath.Join(work, ".terraform", "modules", "net", "main.tf")
			if _, err := os.Stat(installed); err != nil {
				t.Errorf("the module was not installed: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the get ran a program the configuration names: %v", err)
			}
			if _, err := os.Stat(filepath.Join(work, ".terraform", "providers")); !errors.Is(err,
				fs.ErrNotExist) {
				t.Errorf("the get installed providers: %v", err)
			}
		})
	}
}

// TestAPlanOverModulesInPlaceDownloadsNothing holds the real tools to what pinning a run's modules
// relies on: told the modules are already in place, init installs none. A plan over a module tree
// put in place then runs with the module's source gone, so nothing was downloaded, and the same
// plan with no tree in place fails rather than fetching one, so it can never resolve a version
// again.
//
//nolint:funlen // Test function.
func TestAPlanOverModulesInPlaceDownloadsNothing(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		standDown(t, "git is not on PATH, so no module can be installed from a repository")
	}
	tests := []struct {
		// Tool is the tool planned with.
		Tool string
		// Binary is the executable that tool runs.
		Binary string
	}{{ // Test 0: Terraform.
		Tool: run.ToolTerraform, Binary: "terraform",
	}, { // Test 1: OpenTofu.
		Tool: run.ToolOpenTofu, Binary: "tofu",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(test.Binary); err != nil {
				standDown(t, "%s is not on PATH, so the real plan cannot run", test.Binary)
			}
			repo := fetchModuleRepo(t)
			config := fmt.Sprintf("module \"net\" {\n  source = \"git::file://%s?ref=v1.0.0\"\n}\n"+
				"output \"o\" {\n  value = module.net.o\n}\n", filepath.ToSlash(repo))
			installed, bare := t.TempDir(), t.TempDir()
			for _, dir := range []string{installed, bare} {
				if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			fetcher, _ := NewAnsibleRunner().(ModuleFetcher)
			if res, err := fetcher.FetchModules(context.Background(), Spec{Tool: test.Tool,
				Command: ".", Dir: installed}, io.Discard); err != nil || res.ExitCode != 0 {
				t.Fatalf("FetchModules() = %d, %v", res.ExitCode, err)
			}
			// While the source is still there to download from, a plan with no modules in place does
			// not go ahead, since init downloads nothing.
			runner := NewAnsibleRunner()
			var out bytes.Buffer
			res, err := runner.Run(context.Background(), Spec{Tool: test.Tool, Command: ".", Dir: bare,
				DryRun: true, ModulesInstalled: true}, &out)
			if err == nil && res.ExitCode == 0 {
				t.Errorf("a plan with no modules in place went ahead, so init fetched them:\n%s",
					out.String())
			}
			if _, serr := os.Stat(filepath.Join(bare, ".terraform", "modules", "net")); serr == nil {
				t.Error("init installed a module it was told was already in place")
			}
			// With the source gone, the plan over the modules in place runs on what is there.
			if err := os.RemoveAll(repo); err != nil {
				t.Fatalf("remove the module's source: %v", err)
			}
			out.Reset()
			res, err = runner.Run(context.Background(), Spec{Tool: test.Tool, Command: ".",
				Dir: installed, DryRun: true, ModulesInstalled: true}, &out)
			if err != nil || res.ExitCode != 0 || !strings.Contains(out.String(), "o = 1") {
				t.Errorf("plan over the modules in place = %d, %v, want the module's output planned "+
					"with its source gone:\n%s", res.ExitCode, err, out.String())
			}
		})
	}
}

// TestFetchModulesRefusesWhatHasNoModules covers the refusals: a tool that calls no modules, and a
// working directory that is not there.
func TestFetchModulesRefusesWhatHasNoModules(t *testing.T) {
	t.Parallel()
	fetcher, _ := NewAnsibleRunner().(ModuleFetcher)
	tests := []struct {
		// Spec is what the download is asked to fetch for.
		Spec Spec
		// Want is the refusal.
		Want error
	}{{ // Test 0: Ansible calls no modules.
		Spec: Spec{Tool: run.ToolAnsible, Playbook: "site.yml"}, Want: ErrUnknownTool,
	}, { // Test 1: A working directory that is not there.
		Spec: Spec{Tool: run.ToolTerraform, Command: filepath.Join(t.TempDir(), "absent")},
		Want: ErrNoWorkDir,
	}, { // Test 2: No working directory at all.
		Spec: Spec{Tool: run.ToolOpenTofu}, Want: ErrNoCommand,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := fetcher.FetchModules(context.Background(), test.Spec, io.Discard); !errors.Is(err,
				test.Want) {
				t.Errorf("FetchModules() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestAContainerModuleFetchRunsOnlyTheGet pins the container form of the download: inside the
// image the plan would run in, the tool's get alone, in the mounted working directory, with the
// credential files the run would mount, and none of the run's variables.
func TestAContainerModuleFetchRunsOnlyTheGet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cred := filepath.Join(t.TempDir(), "token")
	tests := []struct {
		// Tool is the tool fetched for.
		Tool string
		// WantArgv is what the container runs.
		WantArgv []string
	}{{ // Test 0: Terraform.
		Tool: run.ToolTerraform, WantArgv: []string{"sh", "-c", "terraform get -no-color"},
	}, { // Test 1: OpenTofu.
		Tool: run.ToolOpenTofu, WantArgv: []string{"sh", "-c", "tofu get -no-color"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, cleanup, err := buildModulesPlan(Spec{Tool: test.Tool, Command: ".", Dir: dir,
				Image: "registry.example.test/tf:1", ExtraVars: map[string]any{"secret": "s"},
				CredentialFiles: []string{cred}})
			defer cleanup()
			if err != nil {
				t.Fatalf("buildModulesPlan() error = %v", err)
			}
			if diff := cmp.Diff(test.WantArgv, plan.argv); diff != "" {
				t.Errorf("argv mismatch (-want +got):\n%s", diff)
			}
			if plan.workdir != dir || !slices.Equal(plan.extraEnv, []string{"CHECKPOINT_DISABLE=1"}) {
				t.Errorf("workdir %q, extra env %q, want the working directory, no variables, and the "+
					"version check off", plan.workdir, plan.extraEnv)
			}
			wantMounts := []planMount{{path: dir, writable: true}, {path: cred}}
			if diff := cmp.Diff(wantMounts, plan.mounts, cmp.AllowUnexported(planMount{})); diff != "" {
				t.Errorf("mounts mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if _, _, err := buildModulesPlan(Spec{Tool: run.ToolBash, Command: "true"}); !errors.Is(err,
		ErrUnknownTool) {
		t.Errorf("buildModulesPlan(bash) error = %v, want ErrUnknownTool", err)
	}
}

// standDown skips on a machine allowed to lack something, and fails where the suite was demanded
// whole.
func standDown(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and "+format, args...)
	}
	t.Skipf(format, args...)
}
