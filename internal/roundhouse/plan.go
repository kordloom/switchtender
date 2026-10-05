package roundhouse

import (
	"fmt"
	"path/filepath"
	"regexp"
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
	plan, cleanup, err := toolContainerPlan(spec)
	if err != nil {
		return plan, cleanup, err
	}
	// A credential injection writes a file on the host and points an environment variable at it. That
	// variable crosses into the container unchanged, so without the mount the tool is handed a path
	// that does not exist there and the credential simply fails to apply, which reads as a broken
	// credential rather than a missing mount. Every tool can carry one, so this is not the Ansible
	// branch's business alone.
	for _, f := range spec.CredentialFiles {
		plan.mounts = append(plan.mounts, planMount{path: f})
	}
	return plan, cleanup, nil
}

// buildModulesPlan produces the container plan that runs the get of spec's Terraform or OpenTofu
// working directory and nothing else, with the mounts, working directory, and credential files the
// plan of a run of that directory would have.
func buildModulesPlan(spec Spec) (containerPlan, func(), error) {
	noCleanup := func() {}
	tool := run.NormalizeTool(spec.Tool)
	if tool != run.ToolTerraform && tool != run.ToolOpenTofu {
		return containerPlan{}, noCleanup, fmt.Errorf("%w: %s", ErrUnknownTool, spec.Tool)
	}
	plan, cleanup, err := toolContainerPlan(spec)
	if err != nil {
		return plan, cleanup, err
	}
	bin := "terraform"
	if tool == run.ToolOpenTofu {
		bin = "tofu"
	}
	plan.argv = []string{"sh", "-c", bin + " " + strings.Join(terraformGetArgs(), " ")}
	plan.extraEnv = checkpointEnv(spec.Env)
	for _, f := range spec.CredentialFiles {
		plan.mounts = append(plan.mounts, planMount{path: f})
	}
	return plan, cleanup, nil
}

// toolContainerPlan produces the per-tool part of the plan: the argv, the working directory, and the
// mounts the tool's own inputs need.
func toolContainerPlan(spec Spec) (containerPlan, func(), error) {
	noCleanup := func() {}
	switch run.NormalizeTool(spec.Tool) {
	case run.ToolAnsible:
		if spec.Playbook == "" {
			return containerPlan{}, noCleanup, ErrNoPlaybook
		}
		// Off argv before the plan is built, so the vars land in a mounted file rather than in the
		// docker command line, which the container keeps in Config.Cmd for docker inspect to read.
		varsCleanup, err := materializeExtraVars(&spec)
		if err != nil {
			return containerPlan{}, noCleanup, err
		}
		pargs, err := playbookArgs(spec)
		if err != nil {
			varsCleanup()
			return containerPlan{}, noCleanup, err
		}
		mounts := []planMount{
			{path: spec.Dir},
			{path: filepath.Dir(spec.Playbook)},
			{path: spec.Inventory},
			{path: spec.PrivateKeyPath},
		}
		for _, vp := range spec.VaultPasswords {
			mounts = append(mounts, planMount{path: vp.Path})
		}
		for _, f := range spec.ExtraVarsFiles {
			mounts = append(mounts, planMount{path: f})
		}
		// The fact cache is read and written by the play, so it is the one Ansible input mounted
		// writable. Without the mount the plugin points at a path the container does not have.
		if spec.FactCacheDir != "" {
			mounts = append(mounts, planMount{path: spec.FactCacheDir, writable: true})
		}
		return containerPlan{
			argv:    append([]string{"ansible-playbook"}, pargs...),
			workdir: spec.Dir,
			mounts:  mounts,
		}, varsCleanup, nil
	case run.ToolBash:
		// Same shape as the other three script tools, so the script is a mounted file rather than
		// part of the container's recorded command line.
		return scriptToolPlan(spec, "switchtender-sh-*.sh", func(p string) []string {
			return append([]string{"bash"}, bashArgs(p, spec.DryRun)...)
		})
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
		// the same argument lists as the host runner. Every argument is a fixed literal or a path the
		// executor chose, never input, and each is quoted for the shell.
		script := bin + " " + shellJoin(terraformInitArgs(spec.ModulesInstalled)) + " && " +
			bin + " " + shellJoin(terraformActionArgs(spec))
		mounts := []planMount{{path: dir, writable: true}}
		// A plan saves its plan file into the run's private directory and an apply reads one from it,
		// so that directory's plan location is mounted for the tool: writable for the plan that saves
		// it, read-only for the apply that carries it out.
		if spec.DryRun && spec.PlanOut != "" {
			mounts = append(mounts, planMount{path: filepath.Dir(spec.PlanOut), writable: true})
		}
		if !spec.DryRun && spec.PlanFile != "" {
			mounts = append(mounts, planMount{path: spec.PlanFile})
		}
		return containerPlan{
			argv:     []string{"sh", "-c", script},
			workdir:  dir,
			mounts:   mounts,
			extraEnv: append(checkpointEnv(spec.Env), terraformVars(spec.ExtraVars)...),
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
	path, cleanup, err := writeScriptFileIn(spec.RunDir, pattern, spec.Command)
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

// isBuiltinTool reports whether tool is one of the engines the container runner can execute inside
// an image. It asks the run package rather than restating the set, so a tool added there is a tool
// this classifier already knows.
func isBuiltinTool(tool string) bool {
	return run.IsBuiltinTool(tool)
}

// isTerraformTool reports whether tool runs Terraform or OpenTofu, whose dry run distinguishes drift
// with a detailed plan exit code.
func isTerraformTool(tool string) bool {
	t := run.NormalizeTool(tool)
	return t == run.ToolTerraform || t == run.ToolOpenTofu
}

// buildShowPlan produces the container plan that renders a saved Terraform or OpenTofu plan file as
// JSON and nothing else, with the working directory the plan was made in, its providers, and the
// plan file mounted read-only. Its standard output is the rendering, which the caller captures.
func buildShowPlan(spec Spec, planFile string) (containerPlan, func(), error) {
	noCleanup := func() {}
	tool := run.NormalizeTool(spec.Tool)
	if tool != run.ToolTerraform && tool != run.ToolOpenTofu {
		return containerPlan{}, noCleanup, fmt.Errorf("%w: %s", ErrUnknownTool, spec.Tool)
	}
	dir, err := toolWorkDir(spec.Dir, spec.Command)
	if err != nil {
		return containerPlan{}, noCleanup, err
	}
	bin := "terraform"
	if tool == run.ToolOpenTofu {
		bin = "tofu"
	}
	mounts := []planMount{{path: dir}, {path: planFile}}
	for _, f := range spec.CredentialFiles {
		mounts = append(mounts, planMount{path: f})
	}
	return containerPlan{
		argv:     append([]string{bin}, terraformShowArgs(planFile)...),
		workdir:  dir,
		mounts:   mounts,
		extraEnv: checkpointEnv(spec.Env),
	}, noCleanup, nil
}

// shellSafe matches an argument a POSIX shell reads as itself, which needs no quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+,-]+$`)

// shellJoin joins arguments for a POSIX shell, quoting each one the shell would otherwise split or
// expand, such as a path holding a space.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			quoted[i] = a
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}
