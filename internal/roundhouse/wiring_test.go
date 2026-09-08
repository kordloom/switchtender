package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestNewAnsibleRunnerInstallsTheClosedDefaults pins what the plain constructor actually builds,
// rather than what a rule-level test of the same helpers would prove. A constructor that installed
// a permissive container policy would pass every test of the container rules while still shipping an
// executor that runs somebody's image with no gate on it, so the settings are checked here directly.
func TestNewAnsibleRunnerInstallsTheClosedDefaults(t *testing.T) {
	t.Parallel()
	r, ok := NewAnsibleRunner().(*toolRouter)
	if !ok {
		t.Fatalf("NewAnsibleRunner() = %T, want the tool router", r)
	}
	if r.allowContainer {
		t.Error("container execution is enabled by default, so an image-bound run is never refused")
	}
	if r.container.runtime != "docker" {
		t.Errorf("runtime = %q, want docker", r.container.runtime)
	}
	if r.container.pullPolicy != "missing" {
		t.Errorf("pull policy = %q, want missing", r.container.pullPolicy)
	}
	if r.container.requireDigest {
		t.Error("digest pinning defaults on, which the selective constructor is meant to choose")
	}
	if diff := cmp.Diff(DefaultContainerLimits(), r.container.limits); diff != "" {
		t.Errorf("limits mismatch (-want +got):\n%s", diff)
	}

	// The rule the constructor installs, reached the way a run reaches it.
	res, err := r.Run(context.Background(), Spec{Playbook: "p.yml", Image: "alpine"}, io.Discard)
	if !errors.Is(err, ErrContainerDisabled) {
		t.Errorf("an image-bound run error = %v, want ErrContainerDisabled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestNewSelectiveRunnerCarriesEveryPolicyThrough pins that each argument reaches the container
// runner instead of being dropped on the way. A dropped requireDigest silently permits a mutable
// tag, and dropped limits silently remove the memory, CPU, process and network caps, and neither
// shows up anywhere except in what the container is finally allowed to do.
func TestNewSelectiveRunnerCarriesEveryPolicyThrough(t *testing.T) {
	t.Parallel()
	limits := ContainerLimits{Memory: "512m", CPUs: "0.5", PidsLimit: 64, Network: "none"}
	r, ok := NewSelectiveRunner(true, "podman", "always", true, limits).(*toolRouter)
	if !ok {
		t.Fatalf("NewSelectiveRunner() = %T, want the tool router", r)
	}
	if !r.allowContainer {
		t.Error("allowContainer did not reach the router, so an enabled executor still refuses")
	}
	if r.container.runtime != "podman" {
		t.Errorf("runtime = %q, want podman", r.container.runtime)
	}
	if r.container.pullPolicy != "always" {
		t.Errorf("pull policy = %q, want always", r.container.pullPolicy)
	}
	if !r.container.requireDigest {
		t.Error("digest pinning did not reach the container runner, so a mutable tag is accepted")
	}
	if diff := cmp.Diff(limits, r.container.limits); diff != "" {
		t.Errorf("limits mismatch (-want +got):\n%s", diff)
	}

	// The pinning rule the constructor installed, reached through a run.
	if _, err := r.Run(context.Background(),
		Spec{Playbook: "p.yml", Image: "quay.io/x/y:latest"}, io.Discard); !errors.Is(err,
		ErrUnpinnedImage) {
		t.Errorf("a tag-only image error = %v, want ErrUnpinnedImage", err)
	}
}

// TestToolRouterInstallsTheRightBinaryForEveryTool pins the constructor's per-tool wiring. A router
// that pointed OpenTofu at the terraform binary, or PowerShell at bash, would pass every rule-level
// test of the runners it holds while executing a project's plan with the wrong engine.
func TestToolRouterInstallsTheRightBinaryForEveryTool(t *testing.T) {
	t.Parallel()
	r := newToolRouter(false, "docker", "missing", false, DefaultContainerLimits())
	tests := []struct {
		// Name is the tool whose binary is being checked.
		Name string
		// Got is the binary the router installed for it.
		Got string
		// WantBinary is the executable that tool must resolve to.
		WantBinary string
	}{
		{"ansible", r.binary, "ansible-playbook"},      // Test 0.
		{"bash", r.bash.binary, "bash"},                // Test 1.
		{"terraform", r.terraform.binary, "terraform"}, // Test 2.
		{"opentofu", r.opentofu.binary, "tofu"},        // Test 3.
		{"python", r.python.binary, "python3"},         // Test 4.
		{"powershell", r.powershell.binary, "pwsh"},    // Test 5.
		{"go", r.golang.binary, "go"},                  // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantBinary, test.Got); diff != "" {
				t.Errorf("%s binary mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestToolRouterSharesOneCallbackPluginCache pins that the container runner watches the same plugin
// copy the host runner does. Two caches would mean two directories, and the integrity check that
// restores a rewritten plugin before each use would only ever cover the half of the runs that went
// through the cache it belongs to.
func TestToolRouterSharesOneCallbackPluginCache(t *testing.T) {
	t.Parallel()
	r := newToolRouter(true, "docker", "missing", false, DefaultContainerLimits())
	if r.container.plugin != &r.plugin {
		t.Error("the container runner holds its own plugin cache, so a host run and a container run " +
			"materialize and check different copies of the callback plugin")
	}
}

// TestWithBinaryOverridesOnlyAnsible pins that the option changes the Ansible executable and nothing
// else. An option that leaked into the other runners would let a test binary, or an operator's
// override, silently become the interpreter for every script tool.
func TestWithBinaryOverridesOnlyAnsible(t *testing.T) {
	t.Parallel()
	r := newToolRouter(false, "docker", "missing", false, DefaultContainerLimits(),
		WithBinary("/opt/custom/ansible-playbook"))
	if r.binary != "/opt/custom/ansible-playbook" {
		t.Errorf("ansible binary = %q, want the override", r.binary)
	}
	for name, got := range map[string]string{
		"bash": r.bash.binary, "terraform": r.terraform.binary, "opentofu": r.opentofu.binary,
		"python": r.python.binary, "powershell": r.powershell.binary, "go": r.golang.binary,
	} {
		if strings.Contains(got, "ansible") {
			t.Errorf("%s binary = %q, want the override confined to Ansible", name, got)
		}
	}
}

// TestRouterDispatchesEveryToolToItsOwnRunner pins the dispatch table. Every case is reached by the
// refusal the target runner makes on a Spec missing its input, so routing is proven without any of
// the tool binaries installed. A tool routed to the wrong runner would execute a project's source
// with an engine it was never written for.
func TestRouterDispatchesEveryToolToItsOwnRunner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which route is being proven.
		Name string
		// Spec is the run, deliberately missing the input its tool requires.
		Spec Spec
		// Want is the refusal that only that tool's runner produces.
		Want error
	}{
		{"empty tool means ansible", Spec{}, ErrNoPlaybook},                             // Test 0.
		{"ansible", Spec{Tool: run.ToolAnsible}, ErrNoPlaybook},                         // Test 1.
		{"bash", Spec{Tool: run.ToolBash}, ErrNoCommand},                                // Test 2.
		{"terraform", Spec{Tool: run.ToolTerraform}, ErrNoCommand},                      // Test 3.
		{"opentofu", Spec{Tool: run.ToolOpenTofu}, ErrNoCommand},                        // Test 4.
		{"python", Spec{Tool: run.ToolPython}, ErrNoCommand},                            // Test 5.
		{"powershell", Spec{Tool: run.ToolPowerShell}, ErrNoCommand},                    // Test 6.
		{"go", Spec{Tool: run.ToolGo}, ErrNoCommand},                                    // Test 7.
		{"unknown tool", Spec{Tool: "cobol", Command: "x"}, ErrUnknownTool},             // Test 8.
		{"tool names are exact", Spec{Tool: "Bash", Command: "x"}, ErrUnknownTool},      // Test 9.
		{"whitespace is not a tool", Spec{Tool: " bash", Command: "x"}, ErrUnknownTool}, // Test 10.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			res, err := NewAnsibleRunner().Run(context.Background(), test.Spec, io.Discard)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: Run() error = %v, want %v", test.Name, err, test.Want)
			}
			if res.ExitCode != -1 {
				t.Errorf("%s: ExitCode = %d, want -1", test.Name, res.ExitCode)
			}
		})
	}
}

// TestRouterSendsEveryContainerizableToolToTheContainer pins that pinning an image containerizes
// every built-in engine, not only Ansible. A tool that fell through to the host runner while its
// Spec named an image would execute on the executor itself with the project's credentials, which is
// the isolation the image was chosen for.
func TestRouterSendsEveryContainerizableToolToTheContainer(t *testing.T) {
	t.Parallel()
	tools := []string{
		"", run.ToolAnsible, run.ToolBash, run.ToolTerraform, run.ToolOpenTofu,
		run.ToolPython, run.ToolPowerShell, run.ToolGo,
	}
	for testNum, tool := range tools {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// With container execution off, only a Spec the router sent to the container runner can
			// come back with this refusal, so it identifies the branch taken.
			spec := Spec{Tool: tool, Playbook: "p.yml", Command: "x", Image: "alpine"}
			_, err := NewAnsibleRunner().Run(context.Background(), spec, io.Discard)
			if !errors.Is(err, ErrContainerDisabled) {
				t.Errorf("tool %q with an image: error = %v, want ErrContainerDisabled", tool, err)
			}
		})
	}
}

// TestContainerLimitsArgs pins the docker flags the caps become, including the omissions. A cap that
// silently disappeared would let one container exhaust the host's memory, CPU, or process table, and
// a network value that disappeared would put a run on the runtime's default network rather than the
// one the operator chose.
func TestContainerLimitsArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Limits are the caps under test.
		Limits ContainerLimits
		// WantArgs is the exact flag list they render to.
		WantArgs []string
	}{{ // Test 0: The shipped defaults render every cap.
		Name: "defaults", Limits: DefaultContainerLimits(),
		WantArgs: []string{"--memory", "2g", "--cpus", "2", "--pids-limit", "2048",
			"--network", "bridge"},
	}, { // Test 1: A zero value omits its cap rather than passing an empty flag.
		Name: "empty", Limits: ContainerLimits{}, WantArgs: nil,
	}, { // Test 2: A non-positive process cap is omitted, since docker rejects zero.
		Name: "zero pids", Limits: ContainerLimits{PidsLimit: 0}, WantArgs: nil,
	}, { // Test 3: A negative process cap is omitted too.
		Name: "negative pids", Limits: ContainerLimits{PidsLimit: -1}, WantArgs: nil,
	}, { // Test 4: A cap of one process is honored, since it is positive.
		Name: "one pid", Limits: ContainerLimits{PidsLimit: 1},
		WantArgs: []string{"--pids-limit", "1"},
	}, { // Test 5: An isolated network is passed through, which is the strictest setting.
		Name: "no network", Limits: ContainerLimits{Network: "none"},
		WantArgs: []string{"--network", "none"},
	}, { // Test 6: Caps render in a fixed order, so a command line is reproducible.
		Name: "partial", Limits: ContainerLimits{Memory: "1g", Network: "none"},
		WantArgs: []string{"--memory", "1g", "--network", "none"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantArgs, test.Limits.args()); diff != "" {
				t.Errorf("%s: args mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestDefaultContainerLimitsAreBounded pins the shipped default values themselves. They are the only
// thing standing between a foot-gun playbook and an exhausted host, and a change to any of them,
// especially a network of "host", is a change to what a container can reach.
func TestDefaultContainerLimitsAreBounded(t *testing.T) {
	t.Parallel()
	want := ContainerLimits{Memory: "2g", CPUs: "2", PidsLimit: 2048, Network: "bridge"}
	if diff := cmp.Diff(want, DefaultContainerLimits()); diff != "" {
		t.Errorf("default limits mismatch (-want +got):\n%s", diff)
	}
	if slices.Contains(DefaultContainerLimits().args(), "host") {
		t.Error("the default network is the host's own, so a container shares the executor's stack")
	}
}

// TestRunnerFuncAdaptsAFunction pins the adapter every extension runner is registered through, so a
// function value satisfies Runner and its result and error are returned unchanged.
func TestRunnerFuncAdaptsAFunction(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	var got Spec
	var r Runner = RunnerFunc(func(_ context.Context, spec Spec, out io.Writer) (Result, error) {
		got = spec
		_, _ = io.WriteString(out, "ran")
		return Result{ExitCode: 9, Drift: true}, sentinel
	})

	var buf strings.Builder
	res, err := r.Run(context.Background(), Spec{Tool: "custom", Command: "deploy"}, &buf)

	if !errors.Is(err, sentinel) {
		t.Errorf("Run() error = %v, want the sentinel returned unchanged", err)
	}
	if diff := cmp.Diff(Result{ExitCode: 9, Drift: true}, res); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
	if got.Command != "deploy" || buf.String() != "ran" {
		t.Errorf("the adapter did not pass the spec and writer through: %+v %q", got, buf.String())
	}
}
