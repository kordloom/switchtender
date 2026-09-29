package roundhouse

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kordloom/switchtender/internal/run"
)

// containerPlan describes how to run a Spec inside a container image independent of the host: the
// argv to execute, the working directory, the host paths to bind mount, and any tool-specific
// environment beyond the Spec's own Env. Every containerizable tool produces one, so one container
// code path serves all seven engines instead of only Ansible.
type containerPlan struct {
	// argv is the command run inside the image, for example ansible-playbook or terraform.
	argv []string
	// workdir is the container working directory, mapped to a mounted host path.
	workdir string
	// mounts are the host paths bind mounted into the container at the same path.
	mounts []planMount
	// extraEnv holds tool-specific KEY=VALUE entries added to the run environment, such as
	// SWITCHTENDER_VARS for scripts or TF_VAR_ entries for Terraform.
	extraEnv []string
}

// planMount is a host path bind mounted into a container. Writable is set only for a path the tool
// must write, such as a Terraform working directory holding provider plugins and state.
type planMount struct {
	// path is the host path mounted at the same path inside the container.
	path string
	// writable mounts the path read-write instead of read-only.
	writable bool
}

// buildContainerPlan produces the container plan for spec, writing any temp file the tool needs and
// returning a cleanup that removes it. Each built-in engine is containerizable; a tool missing its
// input returns ErrNoPlaybook or ErrNoCommand, and an unrecognized tool returns ErrUnknownTool.
func buildContainerPlan(spec Spec) (containerPlan, func(), error) {
	noCleanup := func() {}
	switch run.NormalizeTool(spec.Tool) {
	case run.ToolAnsible:
		if spec.Playbook == "" {
			return containerPlan{}, noCleanup, ErrNoPlaybook
		}
		pargs, err := playbookArgs(spec)
		if err != nil {
			return containerPlan{}, noCleanup, err
		}
		mounts := []planMount{
			{path: spec.Dir},
			{path: filepath.Dir(spec.Playbook)},
			{path: spec.Inventory},
			{path: spec.PrivateKeyPath},
			{path: spec.VaultPasswordFile},
		}
		for _, f := range spec.ExtraVarsFiles {
			mounts = append(mounts, planMount{path: f})
		}
		return containerPlan{
			argv:    append([]string{"ansible-playbook"}, pargs...),
			workdir: spec.Dir,
			mounts:  mounts,
		}, noCleanup, nil
	case run.ToolBash:
		if spec.Command == "" {
			return containerPlan{}, noCleanup, ErrNoCommand
		}
		return containerPlan{
			argv:     append([]string{"bash"}, bashArgs(spec)...),
			workdir:  spec.Dir,
			mounts:   []planMount{{path: spec.Dir}},
			extraEnv: varsExtra(spec),
		}, noCleanup, nil
	case run.ToolTerraform, run.ToolOpenTofu:
		if spec.Command == "" {
			return containerPlan{}, noCleanup, ErrNoCommand
		}
		bin := "terraform"
		if run.NormalizeTool(spec.Tool) == run.ToolOpenTofu {
			bin = "tofu"
		}
		dir, err := toolWorkDir(spec.Dir, spec.Command)
		if err != nil {
			return containerPlan{}, noCleanup, err
		}
		// Terraform is a two-phase run, so it goes through a shell: init, then apply or plan, sharing
		// the same argument lists as the host runner. All arguments are fixed literals, not input.
		script := bin + " " + strings.Join(terraformInitArgs(), " ") + " && " +
			bin + " " + strings.Join(terraformActionArgs(spec.DryRun), " ")
		return containerPlan{
			argv:     []string{"sh", "-c", script},
			workdir:  dir,
			mounts:   []planMount{{path: dir, writable: true}},
			extraEnv: terraformVars(spec.ExtraVars),
		}, noCleanup, nil
	case run.ToolPython:
		return scriptToolPlan(spec, "switchtender-py-*.py", func(p string) []string {
			return append([]string{"python3"}, pythonArgs(p, spec.DryRun)...)
		})
	case run.ToolGo:
		return scriptToolPlan(spec, "switchtender-go-*.go", func(p string) []string {
			return append([]string{"go"}, goArgs(p, spec.DryRun)...)
		})
	case run.ToolPowerShell:
		return scriptToolPlan(spec, "switchtender-ps-*.ps1", func(p string) []string {
			return append([]string{"pwsh"}, pwshArgs(p, spec.DryRun)...)
		})
	default:
		return containerPlan{}, noCleanup, fmt.Errorf("%w: %s", ErrUnknownTool, spec.Tool)
	}
}

// scriptToolPlan writes a script tool's inline source to a temp file, then builds a plan that mounts
// the file and the checkout read-only and runs argv, which references the mounted path. It is shared
// by the Python, Go, and PowerShell cases.
func scriptToolPlan(spec Spec, pattern string, argv func(path string) []string) (containerPlan, func(), error) {
	if spec.Command == "" {
		return containerPlan{}, func() {}, ErrNoCommand
	}
	path, cleanup, err := writeScriptFile(pattern, spec.Command)
	if err != nil {
		return containerPlan{}, func() {}, err
	}
	return containerPlan{
		argv:     argv(path),
		workdir:  spec.Dir,
		mounts:   []planMount{{path: path}, {path: spec.Dir}},
		extraEnv: varsExtra(spec),
	}, cleanup, nil
}

// isBuiltinTool reports whether tool is one of the seven engines the container runner can execute
// inside an image.
func isBuiltinTool(tool string) bool {
	switch run.NormalizeTool(tool) {
	case run.ToolAnsible, run.ToolBash, run.ToolTerraform, run.ToolOpenTofu,
		run.ToolPython, run.ToolPowerShell, run.ToolGo:
		return true
	default:
		return false
	}
}

// isTerraformTool reports whether tool runs Terraform or OpenTofu, whose dry run distinguishes drift
// with a detailed plan exit code.
func isTerraformTool(tool string) bool {
	t := run.NormalizeTool(tool)
	return t == run.ToolTerraform || t == run.ToolOpenTofu
}
