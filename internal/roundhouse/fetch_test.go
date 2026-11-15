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
	"runtime"
	"slices"
	"strconv"
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

// gateRows is every entry the gate sets itself in a minimal get's environment whose private
// directory is private, each with the gate's value.
func gateRows(private string) []string {
	return []string{"GIT_ALLOW_PROTOCOL=https", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.emptyAuth", "GIT_CONFIG_VALUE_0=false",
		"KRB5CCNAME=FILE:" + filepath.Join(private, "none"), "AWS_EC2_METADATA_DISABLED=true",
		"GCE_METADATA_HOST=metadata.invalid", "HGRCPATH=" + filepath.Join(private, "hgrc")}
}

// TestAGetAskedForAMinimalEnvironmentStartsFromItAlone holds the approval gate's module download to
// what it may see. A get asked for a minimal environment keeps only the search path, the proxy
// settings, and the trusted certificates' location of the runner's base environment, the server's
// own, and gets a HOME of its own inside the private directory, so a cloud credential, a registry
// token, an SSH agent, or a CLI configuration the server's environment carries never reaches it.
// The gate adds its own rows, which keep git, Mercurial, and the cloud libraries from signing in
// by themselves. Those variables and rows come from the server and the gate alone: a spec naming
// its own, or a base environment setting a gate row to something else, is not heard, and each gate
// row appears once. Without the request a get inherits the whole base environment and no gate row, as
// the run's own get does. A minimal environment with nowhere to put its home is refused, and so is
// one asked for inside an image. A stub stands in for the tool and prints what it was handed.
//
//nolint:funlen,gocognit // Test function.
func TestAGetAskedForAMinimalEnvironmentStartsFromItAlone(t *testing.T) {
	t.Parallel()
	stub := filepath.Join(t.TempDir(), "tf")
	writeStub(t, stub, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\nenv\n")
	kept := []string{"PATH=/usr/bin:/bin", "HTTP_PROXY=http://proxy.example.test:3128",
		"https_proxy=http://proxy.example.test:3128", "NO_PROXY=localhost",
		"SSL_CERT_FILE=/etc/ssl/server.pem", "SSL_CERT_DIR=/etc/ssl/certs"}
	left := []string{"HOME=/home/server", "AWS_PROFILE=prod",
		"GOOGLE_APPLICATION_CREDENTIALS=/etc/gcp.json", "TF_TOKEN_app_terraform_io=token-2468",
		"TF_CLI_CONFIG_FILE=/etc/cli.tfrc", "SSH_AUTH_SOCK=/tmp/agent.sock",
		"GIT_SSH_COMMAND=ssh -i /etc/deploy_key", "GIT_ALLOW_PROTOCOL=ssh:https",
		"GIT_CONFIG_NOSYSTEM=0", "AWS_EC2_METADATA_DISABLED=false",
		"GCE_METADATA_HOST=169.254.169.254", "HGRCPATH=/etc/mercurial/hgrc"}
	base := append(slices.Clone(kept), left...)
	added := []string{"TF_DATA_DIR=.terraform", "CHECKPOINT_DISABLE=1"}
	theirs := []string{"GIT_ALLOW_PROTOCOL=ssh:https", "GIT_CONFIG_NOSYSTEM=0",
		"GIT_TERMINAL_PROMPT=1", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=store", "KRB5CCNAME=FILE:/tmp/krb5cc_server",
		"AWS_EC2_METADATA_DISABLED=false", "GCE_METADATA_HOST=169.254.169.254",
		"HGRCPATH=/etc/mercurial/hgrc"}
	tests := []struct {
		// Minimal asks for the minimal environment.
		Minimal bool
		// NoRunDir leaves the spec with no private directory.
		NoRunDir bool
		// Env is what the spec's own environment adds beyond TF_DATA_DIR.
		Env []string
		// WantHome is whether the get's HOME is the one inside the private directory.
		WantHome bool
		// WantGateRows is whether the gate's own rows are handed to the get, each once.
		WantGateRows bool
		// WantEnv is every entry the get is handed, HOME and the gate's rows aside.
		WantEnv []string
		// Want is the error, nil for none.
		Want error
	}{{ // Test 0: The minimal environment keeps the path, proxies, and certificates, and nothing else.
		Minimal: true, WantHome: true, WantGateRows: true,
		WantEnv: append(slices.Clone(kept), added...),
	}, { // Test 1: Without the request the whole base environment is inherited, and no gate row.
		WantEnv: append(slices.Clone(base), added...),
	}, { // Test 2: A minimal environment with nowhere to put its home is refused.
		Minimal: true, NoRunDir: true, Want: ErrLaunch,
	}, { // Test 3: A spec cannot name the kept variables or HOME for a minimal environment.
		Minimal: true, WantHome: true, WantGateRows: true,
		Env: []string{"SSL_CERT_FILE=/tmp/theirs.pem", "HTTPS_PROXY=http://theirs.example.test",
			"HOME=/tmp/theirs"},
		WantEnv: append(slices.Clone(kept), added...),
	}, { // Test 4: A spec cannot set a gate row to anything else.
		Minimal: true, WantHome: true, WantGateRows: true, Env: theirs,
		WantEnv: append(slices.Clone(kept), added...),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			private := t.TempDir()
			spec := Spec{Tool: run.ToolTerraform, Command: ".", Dir: t.TempDir(), DryRun: true,
				MinimalEnv: test.Minimal, RunDir: private,
				Env: append([]string{"TF_DATA_DIR=.terraform"}, test.Env...)}
			if test.NoRunDir {
				spec.RunDir = ""
			}
			runner := &terraformRunner{binary: stub, baseEnv: base}
			var out bytes.Buffer
			_, err := runner.fetchModules(t.Context(), spec, &out)
			if !errors.Is(err, test.Want) {
				t.Fatalf("fetchModules() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			var got []string
			home := ""
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				name, value, _ := strings.Cut(line, "=")
				switch {
				case slices.Contains([]string{"PWD", "OLDPWD", "SHLVL", "_"}, name):
					// The shell running the stub sets these itself.
				case name == "HOME" && test.WantHome:
					home = value
				default:
					got = append(got, line)
				}
			}
			slices.Sort(got)
			wantEnv := slices.Clone(test.WantEnv)
			if test.WantGateRows {
				wantEnv = append(wantEnv, gateRows(private)...)
			}
			want := slices.Sorted(slices.Values(wantEnv))
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("the get's environment mismatch (-want +got):\n%s", diff)
			}
			if test.WantHome && home != filepath.Join(private, "home") {
				t.Errorf("HOME = %q, want the empty directory inside the private directory %q",
					home, filepath.Join(private, "home"))
			}
			if !test.WantGateRows {
				return
			}
			// The process sees one value per name whatever it is handed, so the list itself is
			// checked for a gate row given twice.
			env, err := runner.getEnv(spec)
			if err != nil {
				t.Fatalf("getEnv() error = %v", err)
			}
			for _, row := range gateRows(private) {
				name, _, _ := strings.Cut(row, "=")
				var given []string
				for _, kv := range env {
					if strings.HasPrefix(kv, name+"=") {
						given = append(given, kv)
					}
				}
				if diff := cmp.Diff([]string{row}, given); diff != "" {
					t.Errorf("%s mismatch (-want +got):\n%s", name, diff)
				}
			}
			hgrc := filepath.Join(private, "hgrc")
			content, err := os.ReadFile(hgrc)
			if err != nil || string(content) != "[ui]\nssh = false\n" {
				t.Errorf("%s = %q, %v, want Mercurial told to run false in place of ssh", hgrc,
					content, err)
			}
			if info, err := os.Stat(hgrc); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, %v, want 0600", hgrc, info, err)
			}
		})
	}
	fetcher, ok := NewSelectiveRunner(true, "docker", "missing", false,
		DefaultContainerLimits()).(ModuleFetcher)
	if !ok {
		t.Fatal("the runner cannot fetch modules")
	}
	_, err := fetcher.FetchModules(t.Context(), Spec{Tool: run.ToolTerraform, Command: ".",
		Dir: t.TempDir(), RunDir: t.TempDir(), MinimalEnv: true,
		Image: "registry.example.test/tf@sha256:" + strings.Repeat("a", 64)}, io.Discard)
	if !errors.Is(err, ErrLaunch) {
		t.Errorf("FetchModules() in an image with a minimal environment = %v, want %v", err,
			ErrLaunch)
	}
}

// gateGetEnv returns the environment the gate's get starts with in a new private directory, on a
// server whose search path finds a stand-in ssh first, with the entry named without left out. The
// stand-in writes marker and fails, so a test sees whether anything started ssh.
func gateGetEnv(t *testing.T, without string) (env []string, private, marker string) {
	t.Helper()
	bin := t.TempDir()
	marker = filepath.Join(bin, "ssh-started")
	writeStub(t, filepath.Join(bin, "ssh"), "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\n"+
		"touch '"+marker+"'\nexit 255\n")
	private = t.TempDir()
	runner := &terraformRunner{baseEnv: []string{"PATH=" + bin + string(os.PathListSeparator) +
		os.Getenv("PATH")}}
	full, err := runner.getEnv(Spec{MinimalEnv: true, RunDir: private})
	if err != nil {
		t.Fatalf("getEnv() error = %v", err)
	}
	for _, kv := range full {
		if name, _, _ := strings.Cut(kv, "="); name != without {
			env = append(env, kv)
		}
	}
	return env, private, marker
}

// gitAtLeast reports whether the git on PATH is at least version major.minor.
func gitAtLeast(major, minor int) bool {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

// TestTheGatesGetSignsInAsNothing holds the real git, started from the environment the gate's get
// starts with, to what the gate's rows promise, with no network. A credential helper in a system
// configuration is not read, and the host's own configuration names none at any level. Git tries
// no sign-in with an empty user name, which is how it reaches Kerberos or Windows sign-in. An ssh
// source is refused as a transport before ssh starts, and a local repository is refused too. Each
// row has a control that leaves the gate's entry out and sees what it stops.
//
//nolint:funlen // Test function.
func TestTheGatesGetSignsInAsNothing(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the gate downloads nothing on Windows, so its get never starts git there")
	}
	if _, err := exec.LookPath("git"); err != nil {
		standDown(t, "git is not on PATH, so the gate's rows for git cannot be checked")
	}
	if !gitAtLeast(2, 32) {
		standDown(t, "git is older than 2.32, the first to read GIT_CONFIG_SYSTEM and "+
			"GIT_CONFIG_COUNT, so the gate's rows for git cannot be checked")
	}
	repo := fetchModuleRepo(t)
	system := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(system, []byte("[credential]\n\thelper = leak\n"), 0o600); err != nil {
		t.Fatalf("write system configuration: %v", err)
	}
	clones := t.TempDir()
	helper := []string{"config", "--get-all", "credential.helper"}
	overSSH := []string{"ls-remote", "ssh://git@example.invalid/x"}
	cloneTo := func(name string) []string {
		return []string{"clone", "-q", "--bare", "file://" + filepath.ToSlash(repo),
			filepath.Join(clones, name)}
	}
	tests := []struct {
		// Args are git's arguments.
		Args []string
		// Without is the gate's entry left out, empty for none.
		Without string
		// HostSystem leaves git to find this host's own system configuration.
		HostSystem bool
		// WantOut is a line git prints to standard output, among any the host adds, such as the
		// keychain helper Apple's git sets at the system level. Empty wants nothing printed at all.
		WantOut string
		// WantFail is whether git fails.
		WantFail bool
		// WantErr is a fragment of what git prints to standard error, empty for any.
		WantErr string
		// WantSSH is whether ssh started.
		WantSSH bool
	}{{ // Test 0: A credential helper in the system configuration is not read.
		Args: helper, WantFail: true,
	}, { // Test 1: Without the gate's entry it is read, the control.
		Args: helper, Without: "GIT_CONFIG_NOSYSTEM", WantOut: "leak",
	}, { // Test 2: The host's own configuration names no credential helper at any level.
		Args:       []string{"config", "--show-origin", "--get-all", "credential.helper"},
		HostSystem: true, WantFail: true,
	}, { // Test 3: Git tries no sign-in with an empty user name.
		Args: []string{"config", "--get", "http.emptyAuth"}, WantOut: "false",
	}, { // Test 4: Without the gate's entries it would, the control.
		Args: []string{"config", "--get", "http.emptyAuth"}, Without: "GIT_CONFIG_COUNT",
		WantFail: true,
	}, { // Test 5: An ssh source is refused as a transport, and ssh never starts.
		Args: overSSH, WantFail: true, WantErr: "transport 'ssh' not allowed",
	}, { // Test 6: Without the gate's entry ssh starts, the control.
		Args: overSSH, Without: "GIT_ALLOW_PROTOCOL", WantFail: true, WantSSH: true,
	}, { // Test 7: A local repository is refused as a transport.
		Args: cloneTo("refused"), WantFail: true, WantErr: "transport 'file' not allowed",
	}, { // Test 8: Without the gate's entry it is cloned, the control.
		Args: cloneTo("cloned"), Without: "GIT_ALLOW_PROTOCOL",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			env, _, marker := gateGetEnv(t, test.Without)
			if !test.HostSystem {
				env = append(env, "GIT_CONFIG_SYSTEM="+system)
			}
			cmd := exec.Command("git", test.Args...)
			cmd.Dir = t.TempDir()
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if failed := err != nil; failed != test.WantFail {
				t.Errorf("git %s failed = %v, want %v: %s", strings.Join(test.Args, " "), failed,
					test.WantFail, stderr.String())
			}
			printed := strings.TrimSpace(stdout.String())
			if test.WantOut == "" && printed != "" || test.WantOut != "" &&
				!slices.Contains(strings.Split(printed, "\n"), test.WantOut) {
				t.Errorf("git %s printed %q, want %q", strings.Join(test.Args, " "), printed,
					test.WantOut)
			}
			if !strings.Contains(stderr.String(), test.WantErr) {
				t.Errorf("git %s said %q, want %q", strings.Join(test.Args, " "), stderr.String(),
					test.WantErr)
			}
			_, serr := os.Stat(marker)
			if started := serr == nil; started != test.WantSSH {
				t.Errorf("ssh started = %v, want %v", started, test.WantSSH)
			}
		})
	}
}

// TestTheGatesGetKeepsMercurialOffSSH holds Mercurial, started from the environment the gate's get
// starts with, to the file the gate writes for it: it reads that file alone, which runs false in
// place of ssh, so an ssh source never starts ssh. Leaving the gate's entry out is the control,
// where ssh starts. With no Mercurial installed only the file is checked, which the minimal
// environment test does too.
func TestTheGatesGetKeepsMercurialOffSSH(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the gate downloads nothing on Windows, so its get never starts Mercurial there")
	}
	tests := []struct {
		// Without is the gate's entry left out, empty for none.
		Without string
		// WantSSH is whether ssh started.
		WantSSH bool
	}{{ // Test 0: An ssh source never starts ssh.
		WantSSH: false,
	}, { // Test 1: Without the gate's entry ssh starts, the control.
		Without: "HGRCPATH", WantSSH: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			env, private, marker := gateGetEnv(t, test.Without)
			hgrc := filepath.Join(private, "hgrc")
			if content, err := os.ReadFile(hgrc); err != nil || string(content) != "[ui]\nssh = false\n" {
				t.Errorf("%s = %q, %v, want Mercurial told to run false in place of ssh", hgrc,
					content, err)
			}
			if named := slices.Contains(env, "HGRCPATH="+hgrc); named != (test.Without == "") {
				t.Errorf("HGRCPATH names %s = %v, want %v", hgrc, named, test.Without == "")
			}
			// Mercurial is optional on a server, and the suite does not require it.
			if _, err := exec.LookPath("hg"); err != nil {
				t.Skip("Mercurial is not on PATH, so only the file it would read is checked")
			}
			cmd := exec.Command("hg", "identify", "ssh://example.invalid/x")
			cmd.Dir = t.TempDir()
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("hg identify of an ssh source succeeded: %s", out)
			}
			_, serr := os.Stat(marker)
			if started := serr == nil; started != test.WantSSH {
				t.Errorf("ssh started = %v, want %v", started, test.WantSSH)
			}
		})
	}
}
