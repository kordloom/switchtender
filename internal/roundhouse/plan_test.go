package roundhouse

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestBuildContainerPlanAnsible checks the Ansible plan runs ansible-playbook with the playbook args
// and mounts the checkout at the working directory.
func TestBuildContainerPlanAnsible(t *testing.T) {
	t.Parallel()
	spec := Spec{
		Tool: "ansible", Playbook: "/co/site.yml", Inventory: "/co/hosts.ini",
		Dir: "/co", PrivateKeyPath: "/co/key", Limit: "web01",
	}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	want := []string{
		"ansible-playbook", "-i", "/co/hosts.ini", "--limit", "web01",
		"--private-key", "/co/key", "--", "/co/site.yml",
	}
	if diff := cmp.Diff(want, plan.argv); diff != "" {
		t.Errorf("argv mismatch (-want +got):\n%s", diff)
	}
	if plan.workdir != "/co" {
		t.Errorf("workdir = %q, want /co", plan.workdir)
	}
}

// TestBuildContainerPlanBash checks the bash plan, its dry-run flag, its single checkout mount, and
// that extra vars surface as SWITCHTENDER_VARS.
func TestBuildContainerPlanBash(t *testing.T) {
	t.Parallel()
	spec := Spec{Tool: "bash", Command: "echo hi", Dir: "/co"}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	// Bash takes a mounted script path like the other three script tools. It used to put the whole
	// script body in argv, which lands in the container's recorded Config.Cmd, where docker inspect
	// reads it back long after the run ended and the temp file is gone.
	if len(plan.argv) != 2 || plan.argv[0] != "bash" {
		t.Fatalf("argv = %v, want bash plus one script path", plan.argv)
	}
	if strings.Contains(strings.Join(plan.argv, " "), "echo hi") {
		t.Errorf("argv = %v, still carries the script body rather than a path to it", plan.argv)
	}
	if len(plan.mounts) != 2 || plan.mounts[0].path != plan.argv[1] || plan.mounts[1].path != "/co" {
		t.Errorf("mounts = %+v, want the script and /co read-only", plan.mounts)
	}
	for _, m := range plan.mounts {
		if m.writable {
			t.Errorf("mount %q is writable, want read-only", m.path)
		}
	}
	if plan.extraEnv != nil {
		t.Errorf("extraEnv = %v, want nil", plan.extraEnv)
	}
	// The script is written where the runner can mount it, and holds exactly what was submitted.
	body, rerr := os.ReadFile(plan.argv[1])
	if rerr != nil {
		t.Fatalf("the plan named a script file that is not there: %v", rerr)
	}
	if string(body) != "echo hi" {
		t.Errorf("script file holds %q, want the submitted command", body)
	}

	dry := Spec{Tool: "bash", Command: "echo hi", Dir: "/co", DryRun: true, ExtraVars: map[string]any{"k": "v"}}
	plan2, cleanup2, err := buildContainerPlan(dry)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup2()
	if len(plan2.argv) != 3 || plan2.argv[0] != "bash" || plan2.argv[1] != "-n" {
		t.Errorf("dry-run argv = %v, want bash -n plus a script path", plan2.argv)
	}
	if len(plan2.extraEnv) != 1 || !strings.HasPrefix(plan2.extraEnv[0], "SWITCHTENDER_VARS=") {
		t.Errorf("extraEnv = %v, want a SWITCHTENDER_VARS entry", plan2.extraEnv)
	}
}

// TestBuildContainerPlanTerraform checks Terraform and OpenTofu run init then the action through a
// shell, in a writable working directory resolved under the checkout.
func TestBuildContainerPlanTerraform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Tool    string
		DryRun  bool
		WantBin string
		WantAct string
	}{
		{"terraform apply", "terraform", false, "terraform", "apply -auto-approve"},
		{"terraform plan", "terraform", true, "terraform", "plan -input=false -no-color -detailed-exitcode"},
		{"opentofu apply", "opentofu", false, "tofu", "apply -auto-approve"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			spec := Spec{Tool: test.Tool, Command: "infra/net", Dir: "/co", DryRun: test.DryRun}
			plan, cleanup, err := buildContainerPlan(spec)
			if err != nil {
				t.Fatalf("buildContainerPlan() error = %v", err)
			}
			defer cleanup()
			if len(plan.argv) != 3 || plan.argv[0] != "sh" || plan.argv[1] != "-c" {
				t.Fatalf("argv = %v, want sh -c <script>", plan.argv)
			}
			script := plan.argv[2]
			if !strings.Contains(script, test.WantBin+" init") ||
				!strings.Contains(script, test.WantBin+" "+test.WantAct) {
				t.Errorf("script = %q, want init and %q", script, test.WantAct)
			}
			if plan.workdir != "/co/infra/net" {
				t.Errorf("workdir = %q, want /co/infra/net", plan.workdir)
			}
			if len(plan.mounts) != 1 || plan.mounts[0].path != "/co/infra/net" || !plan.mounts[0].writable {
				t.Errorf("mounts = %+v, want /co/infra/net writable", plan.mounts)
			}
		})
	}
}

// TestBuildContainerPlanScriptTools checks Python, Go, and PowerShell write a temp script, mount it
// and the checkout read-only, and reference the script path in the argv.
func TestBuildContainerPlanScriptTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Tool     string
		WantHead string
	}{
		{"python", "python3"},
		{"go", "go"},
		{"powershell", "pwsh"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Tool), func(t *testing.T) {
			t.Parallel()
			spec := Spec{Tool: test.Tool, Command: "print", Dir: "/co"}
			plan, cleanup, err := buildContainerPlan(spec)
			if err != nil {
				t.Fatalf("buildContainerPlan() error = %v", err)
			}
			defer cleanup()
			if len(plan.argv) == 0 || plan.argv[0] != test.WantHead {
				t.Fatalf("argv = %v, want head %q", plan.argv, test.WantHead)
			}
			if len(plan.mounts) != 2 || plan.mounts[0].writable || plan.mounts[1].writable {
				t.Fatalf("mounts = %+v, want the script and checkout read-only", plan.mounts)
			}
			if plan.mounts[1].path != "/co" {
				t.Errorf("second mount = %q, want /co", plan.mounts[1].path)
			}
			script := plan.mounts[0].path
			found := false
			for _, a := range plan.argv {
				if strings.Contains(a, script) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("argv %v does not reference the script path %q", plan.argv, script)
			}
		})
	}
}

// TestBuildContainerPlanErrors checks a tool missing its input or an unknown tool is rejected.
func TestBuildContainerPlanErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Spec Spec
		Want error
	}{
		{"ansible no playbook", Spec{Tool: "ansible"}, ErrNoPlaybook},
		{"bash no command", Spec{Tool: "bash"}, ErrNoCommand},
		{"terraform no command", Spec{Tool: "terraform"}, ErrNoCommand},
		{"python no command", Spec{Tool: "python"}, ErrNoCommand},
		{"unknown tool", Spec{Tool: "cobol", Command: "x"}, ErrUnknownTool},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, cleanup, err := buildContainerPlan(test.Spec)
			cleanup()
			if !errors.Is(err, test.Want) {
				t.Errorf("buildContainerPlan() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestNewContainerRunnerRuntime checks the runtime selector honors podman and defaults empty to
// docker.
func TestNewContainerRunnerRuntime(t *testing.T) {
	t.Parallel()
	if c := newContainerRunner("podman", "missing", false, nil, &pluginCache{},
		DefaultContainerLimits()); c.runtime != "podman" {
		t.Errorf("runtime = %q, want podman", c.runtime)
	}
	if c := newContainerRunner("", "missing", false, nil, &pluginCache{},
		DefaultContainerLimits()); c.runtime != "docker" {
		t.Errorf("empty runtime = %q, want docker default", c.runtime)
	}
}

// TestNoScriptToolPutsItsScriptOnArgv pins the property that keeps an inline secret out of ps and
// out of a container's recorded command line.
//
// bash was the only one of the four script engines that ran the script body as an argument, via
// bash -c "<script>". On Linux the process argument list is world-readable, so any other account on
// the executor could read a running job's script out of ps, and a containerized run kept the same
// bytes in Config.Cmd where docker inspect returns them long after the run finished and the temp
// file was gone. An operator who writes a password inline is doing something the docs warn against,
// but three of the four tools did not punish it this way, and the odd one out was not a decision
// anyone made.
func TestNoScriptToolPutsItsScriptOnArgv(t *testing.T) {
	t.Parallel()
	const marker = "hunter2_do_not_leak_this"
	for _, tool := range []string{"bash", "python", "go", "powershell"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			spec := Spec{Tool: tool, Command: "echo " + marker, Dir: "/co"}
			plan, cleanup, err := buildContainerPlan(spec)
			if err != nil {
				t.Fatalf("buildContainerPlan(%s) error = %v", tool, err)
			}
			defer cleanup()
			if joined := strings.Join(plan.argv, " "); strings.Contains(joined, marker) {
				t.Errorf("%s: the script body is on argv, readable from ps and kept in the "+
					"container's Config.Cmd: %s", tool, joined)
			}
			// The script still has to reach the container, as a mounted file.
			var mounted bool
			for _, m := range plan.mounts {
				if body, rerr := os.ReadFile(m.path); rerr == nil && strings.Contains(string(body), marker) {
					mounted = true
				}
			}
			if !mounted {
				t.Errorf("%s: the script is neither on argv nor in a mounted file, so the run "+
					"would execute nothing", tool)
			}
		})
	}
}
