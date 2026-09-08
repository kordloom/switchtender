//go:build unix

package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// stubRuntimeExits writes an executable stand-in for the container CLI and returns its absolute
// path plus the directory holding the evidence it records. Every invocation appends its arguments
// to calls.log and dumps its environment to <subcommand>.env, "login" also saves its standard input
// to login.stdin, and the "run" and "login" subcommands exit with the given statuses. Passing the
// absolute path as the runner's runtime keeps the test off PATH, so it stays parallel-safe.
func stubRuntimeExits(t *testing.T, runExit, loginExit int) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\n" +
		`[ "$1" = stub-warmup ] && exit 0` + "\n" +
		`echo "$@" >> ` + filepath.Join(dir, "calls.log") + "\n" +
		`env > ` + dir + `/"$1".env` + "\n" +
		`case "$1" in` + "\n" +
		`  login) cat > ` + filepath.Join(dir, "login.stdin") + "; exit " +
		strconv.Itoa(loginExit) + " ;;\n" +
		`  run) exit ` + strconv.Itoa(runExit) + " ;;\n" +
		"esac\nexit 0\n"
	bin = filepath.Join(dir, "docker")
	writeStub(t, bin, script)
	return bin, dir
}

// readStubFile returns the contents of one of the stub runtime's evidence files, or empty when the
// stub never wrote it.
func readStubFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// stubBaseEnv is a minimal base environment for a runner under test. It carries PATH so the stub
// shell script can find the utilities it calls, and nothing else, so any variable the stub reports
// came from the code under test rather than from the developer's own environment.
func stubBaseEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH")}
}

// TestContainerRunOutcomes pins how a completed container run is classified, which is what decides
// whether an operator sees a success, a failure, or drift. The Terraform and OpenTofu dry run uses
// plan -detailed-exitcode, where 2 means a clean plan with pending changes; every other combination
// of exit 2 is an ordinary failing run and must not be laundered into a success.
//
//nolint:funlen // Test function.
func TestContainerRunOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Spec is the run, minus the image which every case shares.
		Spec Spec
		// RunExit is the status the stub runtime's "run" subcommand exits with.
		RunExit int
		// WantExit is the exit code the Result must carry.
		WantExit int
		// WantDrift is whether the run must be reported as drift.
		WantDrift bool
	}{{ // Test 0: A clean run is success.
		Name: "clean run", Spec: Spec{Tool: run.ToolBash, Command: "echo hi"},
		RunExit: 0, WantExit: 0,
	}, { // Test 1: A tool's own failure is that exit code, not an executor error.
		Name: "tool failed", Spec: Spec{Tool: run.ToolBash, Command: "echo hi"},
		RunExit: 7, WantExit: 7,
	}, { // Test 2: A Terraform dry run exiting 2 is drift, reported as a successful check.
		Name: "terraform plan drift", Spec: Spec{Tool: run.ToolTerraform, Command: ".", DryRun: true},
		RunExit: 2, WantExit: 0, WantDrift: true,
	}, { // Test 3: OpenTofu shares the detailed exit code, so it shares the drift reading.
		Name: "opentofu plan drift", Spec: Spec{Tool: run.ToolOpenTofu, Command: ".", DryRun: true},
		RunExit: 2, WantExit: 0, WantDrift: true,
	}, { // Test 4: Terraform exiting 2 on an apply is a failure, since only plan means drift by 2.
		Name: "terraform apply exit two", Spec: Spec{Tool: run.ToolTerraform, Command: "."},
		RunExit: 2, WantExit: 2,
	}, { // Test 5: A bash dry run exiting 2 is a syntax failure, never drift.
		Name: "bash dry run exit two", Spec: Spec{Tool: run.ToolBash, Command: "if", DryRun: true},
		RunExit: 2, WantExit: 2,
	}, { // Test 6: A python dry run exiting 2 is a failure, since only Terraform reads 2 as drift.
		Name: "python dry run exit two", Spec: Spec{Tool: run.ToolPython, Command: "x", DryRun: true},
		RunExit: 2, WantExit: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bin, _ := stubRuntimeExits(t, test.RunExit, 0)
			c := newContainerRunner(bin, "missing", false, nil, &pluginCache{},
				DefaultContainerLimits())
			spec := test.Spec
			spec.Image = "alpine:3"
			spec.Dir = t.TempDir()
			res, err := c.Run(context.Background(), spec, io.Discard)
			if err != nil {
				t.Fatalf("%s: Run() error = %v", test.Name, err)
			}
			want := Result{ExitCode: test.WantExit, Drift: test.WantDrift}
			if diff := cmp.Diff(want, res); diff != "" {
				t.Errorf("%s: result mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestContainerRunRefusalsHappenBeforeTheRuntimeIsCalled pins that every refusal fails closed: the
// container CLI is never invoked at all, so a rejected image or a Spec missing its input cannot
// start a container by some later path. A refusal that still shelled out would be a refusal only in
// the returned error.
func TestContainerRunRefusalsHappenBeforeTheRuntimeIsCalled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Spec is the run being refused.
		Spec Spec
		// RequireDigest turns on digest pinning for the case that needs it.
		RequireDigest bool
		// Want is the error the refusal must carry.
		Want error
	}{{ // Test 0: An empty image is not a container run.
		Name: "no image", Spec: Spec{Tool: run.ToolBash, Command: "echo hi"}, Want: ErrNoImage,
	}, { // Test 1: A reference the CLI would read as a flag is refused.
		Name: "image reads as a flag", Spec: Spec{Tool: run.ToolBash, Command: "x", Image: "-rm"},
		Want: ErrBadImage,
	}, { // Test 2: Shell metacharacters in a reference are refused.
		Name: "image carries metacharacters",
		Spec: Spec{Tool: run.ToolBash, Command: "x", Image: "alpine;id"}, Want: ErrBadImage,
	}, { // Test 3: With pinning on, a mutable tag is refused.
		Name: "unpinned image", Spec: Spec{Tool: run.ToolBash, Command: "x", Image: "alpine:3"},
		RequireDigest: true, Want: ErrUnpinnedImage,
	}, { // Test 4: Ansible without a playbook never reaches the runtime.
		Name: "no playbook", Spec: Spec{Tool: run.ToolAnsible, Image: "alpine:3"}, Want: ErrNoPlaybook,
	}, { // Test 5: A script tool without its source never reaches the runtime.
		Name: "no command", Spec: Spec{Tool: run.ToolBash, Image: "alpine:3"}, Want: ErrNoCommand,
	}, { // Test 6: An unknown tool never reaches the runtime.
		Name: "unknown tool", Spec: Spec{Tool: "cobol", Command: "x", Image: "alpine:3"},
		Want: ErrUnknownTool,
	}, { // Test 7: A forbidden mount refuses the run rather than dropping the mount.
		Name: "forbidden mount",
		Spec: Spec{Tool: run.ToolAnsible, Playbook: "/etc/site.yml", Image: "alpine:3"},
		Want: ErrForbiddenMount,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bin, dir := stubRuntimeExits(t, 0, 0)
			c := newContainerRunner(bin, "missing", test.RequireDigest, nil, &pluginCache{},
				DefaultContainerLimits())
			res, err := c.Run(context.Background(), test.Spec, io.Discard)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: Run() error = %v, want %v", test.Name, err, test.Want)
			}
			if res.ExitCode != -1 {
				t.Errorf("%s: ExitCode = %d, want -1", test.Name, res.ExitCode)
			}
			if calls := readStubFile(t, dir, "calls.log"); calls != "" {
				t.Errorf("%s: the runtime was invoked despite the refusal:\n%s", test.Name, calls)
			}
		})
	}
}

// TestContainerRunFailsClosedWhenTheRegistryLoginFails pins that a private image whose login is
// rejected never proceeds to the run. Continuing would attempt an anonymous pull of the same
// reference, so a public image squatting that name would execute in place of the private one the
// operator meant, with the run's credentials injected into it.
func TestContainerRunFailsClosedWhenTheRegistryLoginFails(t *testing.T) {
	t.Parallel()
	bin, dir := stubRuntimeExits(t, 0, 1)
	c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
		DefaultContainerLimits())
	spec := Spec{
		Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(),
		Image: "ghcr.io/org/private:1", RegistryUsername: "ci-bot", RegistryPassword: "hunter2",
	}

	res, err := c.Run(context.Background(), spec, io.Discard)

	if !errors.Is(err, ErrLaunch) {
		t.Fatalf("Run() error = %v, want ErrLaunch", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	calls := readStubFile(t, dir, "calls.log")
	if !strings.Contains(calls, "login") {
		t.Fatalf("the login was never attempted:\n%s", calls)
	}
	if strings.Contains(calls, "run --rm") {
		t.Errorf("the image ran after its registry login was rejected, so an anonymous pull of the "+
			"same reference executes instead:\n%s", calls)
	}
}

// TestContainerRunKeepsTheRegistryPasswordOffArgvAndOutOfTheEnvironment pins the two places a
// registry password must never appear. Arguments are readable by any local account through ps for
// the life of the process, and the environment of the pull is inherited by anything it starts, so
// the password travels on standard input alone.
func TestContainerRunKeepsTheRegistryPasswordOffArgvAndOutOfTheEnvironment(t *testing.T) {
	t.Parallel()
	const password = "correct-horse-battery-staple"
	bin, dir := stubRuntimeExits(t, 0, 0)
	c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
		DefaultContainerLimits())
	spec := Spec{
		Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(),
		Image: "ghcr.io/org/private:1", RegistryUsername: "ci-bot", RegistryPassword: password,
	}

	if _, err := c.Run(context.Background(), spec, io.Discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	calls := readStubFile(t, dir, "calls.log")
	if strings.Contains(calls, password) {
		t.Errorf("the registry password is on the command line, where ps shows it to any local "+
			"account:\n%s", calls)
	}
	for _, want := range []string{"login ghcr.io", "-u ci-bot", "--password-stdin"} {
		if !strings.Contains(calls, want) {
			t.Errorf("login arguments %q missing %q", calls, want)
		}
	}
	if got := readStubFile(t, dir, "login.stdin"); got != password {
		t.Errorf("login stdin = %q, want the password delivered there", got)
	}
	for _, name := range []string{"login.env", "run.env"} {
		if strings.Contains(readStubFile(t, dir, name), password) {
			t.Errorf("the registry password reached the %s of the container CLI", name)
		}
	}
}

// TestRegistryLoginIsWrittenToAPerRunDirectoryThatIsRemoved pins that the credential a private pull
// writes exists only for the length of the run. The runtime stores a login in its config directory,
// and when that directory is the executor's own, one project's credential authenticates every later
// pull on the machine, including pulls by a project that was never granted the registry.
func TestRegistryLoginIsWrittenToAPerRunDirectoryThatIsRemoved(t *testing.T) {
	t.Parallel()
	bin, dir := stubRuntimeExits(t, 0, 0)
	c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
		DefaultContainerLimits())
	spec := Spec{
		Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(),
		Image: "ghcr.io/org/private:1", RegistryUsername: "ci-bot", RegistryPassword: "pw",
	}

	if _, err := c.Run(context.Background(), spec, io.Discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	configDir := envValue(readStubFile(t, dir, "login.env"), "DOCKER_CONFIG")
	authFile := envValue(readStubFile(t, dir, "login.env"), "REGISTRY_AUTH_FILE")
	if configDir == "" {
		t.Fatal("the login was not pointed at a private config directory, so it lands in the " +
			"executor's own and serves every later pull")
	}
	// Both runtimes are covered, so the login is scoped whichever one is configured.
	if want := filepath.Join(configDir, "config.json"); authFile != want {
		t.Errorf("REGISTRY_AUTH_FILE = %q, want %q so podman scopes the login too", authFile, want)
	}
	// The pull shares the directory, or the login it just performed would not be visible to it.
	if got := envValue(readStubFile(t, dir, "run.env"), "DOCKER_CONFIG"); got != configDir {
		t.Errorf("the run used config dir %q, want the login's %q", got, configDir)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Errorf("the registry credential outlived its run at %q: %v", configDir, err)
	}
}

// TestContainerRunWithoutCredentialsPerformsNoLogin pins that an ordinary public-image run never
// touches the registry credential path, so no config directory and no login are produced for a run
// that has nothing to authenticate with.
func TestContainerRunWithoutCredentialsPerformsNoLogin(t *testing.T) {
	t.Parallel()
	bin, dir := stubRuntimeExits(t, 0, 0)
	c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
		DefaultContainerLimits())
	spec := Spec{Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(), Image: "alpine:3"}

	if _, err := c.Run(context.Background(), spec, io.Discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if calls := readStubFile(t, dir, "calls.log"); strings.Contains(calls, "login") {
		t.Errorf("a public-image run attempted a registry login:\n%s", calls)
	}
	if got := envValue(readStubFile(t, dir, "run.env"), "DOCKER_CONFIG"); got != "" {
		t.Errorf("DOCKER_CONFIG = %q, want the runtime's own default for a run with no login", got)
	}
}

// TestContainerRunReportsAnUnlaunchableRuntime pins that a missing or unusable container CLI is an
// executor error rather than a tool failure, so an operator is not told their playbook failed when
// the runtime was never there.
func TestContainerRunReportsAnUnlaunchableRuntime(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("switchtender-no-such-runtime", "missing", false, nil, &pluginCache{},
		DefaultContainerLimits())
	spec := Spec{Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(), Image: "alpine:3"}

	res, err := c.Run(context.Background(), spec, io.Discard)

	if !errors.Is(err, ErrLaunch) {
		t.Errorf("Run() error = %v, want ErrLaunch", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestContainerRunGetsNoneOfTheHostEnvironment pins the isolation a container run is chosen for.
// The host runners inherit the executor's environment on purpose, but a containerized run must
// receive only what the Spec injects, so a proxy setting, a stray token, or anything else ambient on
// the executor does not silently become part of somebody else's image.
func TestContainerRunGetsNoneOfTheHostEnvironment(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false,
		[]string{"HOST_ONLY_MARKER=must-not-cross"}, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(), Image: "alpine:3"}

	path, cleanup, err := c.writeEnvFile(spec, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("writeEnvFile() error = %v", err)
	}
	if path != "" {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile() error = %v", readErr)
		}
		t.Fatalf("a run injecting nothing still wrote an environment file:\n%s", body)
	}
}

// envValue reads one variable out of the environment dump the stub runtime wrote, returning empty
// when the variable is unset.
func envValue(dump, name string) string {
	for _, line := range strings.Split(dump, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == name {
			return v
		}
	}
	return ""
}
