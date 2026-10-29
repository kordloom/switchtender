package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// paidModule is where the private paid module is mounted in a checkout that has it. A public clone
// holds an empty directory here.
const paidModule = "internal/paid/impl"

// buildTarget returns the build tags and the main package for the binary and the image. The paid
// build has its own entry point inside the paid module, so no public file names that module.
func buildTarget(paid bool) (tags, main string) {
	if paid {
		return "paid", "./" + paidModule + "/cmd/switchtender"
	}
	return "", "."
}

// checkBuild refuses a run whose phases cannot pass on the build it would test, before any cluster
// is made: the Team phases on the Community build, or a paid build from a checkout without the
// paid module.
func checkBuild(repo string, paid, skipTeam bool) error {
	if !paid {
		if !skipTeam {
			return fmt.Errorf("%w: pass -paid from a checkout with %s mounted, or run with -skip-team "+
				"to test the Community tier alone", ErrTeamNeedsPaid, paidModule)
		}
		return nil
	}
	_, main := buildTarget(true)
	if _, err := os.Stat(filepath.Join(repo, main, "main.go")); err != nil {
		return fmt.Errorf("%w: -paid builds %s, and %s holds no main.go: run git submodule update "+
			"--init %s and copy its build/go.mod and build/go.sum over the root", ErrPaidModuleMissing,
			main, filepath.Join(repo, main), paidModule)
	}
	return nil
}

// previousPaid is the image upgrade-team starts from and, when the harness must build it, the
// source it is built from.
type previousPaid struct {
	// Image is the image reference the phase installs, repository and tag.
	Image string
	// Ref is the public tag the image is built from. Empty means the image is pulled, not built.
	Ref string
	// Commit is the paid module commit the gitlink at Ref names. Empty when Ref is empty.
	Commit string
	// Version is the release version the image carries, without the leading v.
	Version string
}

// resolvePreviousPaid decides where upgrade-team's starting image comes from, before any cluster
// exists. An explicit flagValue wins. Otherwise the previous release is the newest tag older than
// the chart's appVersion. A tag whose tree records no paid module gitlink predates the paid wall,
// and its public image still carries the paid code, so that image is pulled. A tag that records
// one is built from both trees, which needs the gitlink's commit in the local paid module.
func resolvePreviousPaid(flagValue, repo string) (previousPaid, error) {
	if flagValue != "" {
		return previousPaid{Image: flagValue}, nil
	}
	tags, err := releaseTags(repo)
	if err != nil {
		return previousPaid{}, err
	}
	building, err := chartAppVersion(repo)
	if err != nil {
		return previousPaid{}, err
	}
	prev := previousTag(tags, building)
	if prev == "" {
		return previousPaid{}, fmt.Errorf("no release tag is older than %s, which the chart says "+
			"this tree builds; fetch tags or pass -prev-paid-image", building)
	}
	version := strings.TrimPrefix(prev, "v")
	ref, commit, err := gitlinkAt(repo, prev)
	if err != nil {
		return previousPaid{}, err
	}
	if commit == "" {
		return previousPaid{Image: "ghcr.io/kordloom/switchtender:" + version, Version: version}, nil
	}
	module := filepath.Join(repo, paidModule)
	if err := exec.Command("git", "-C", module, "cat-file", "-e", commit+"^{commit}").Run(); err != nil {
		return previousPaid{}, fmt.Errorf("%w: %s records paid module commit %s, which %s does not "+
			"hold; fetch the paid module with its history (git -C %s fetch --unshallow, or fetch-depth "+
			"0 in CI) or pass -prev-paid-image", ErrPreviousPaidMissing, prev, commit, module, paidModule)
	}
	return previousPaid{
		Image: "switchtender-paid:" + version, Ref: ref, Commit: commit, Version: version,
	}, nil
}

// gitlinkAt returns the tag ref holding release tag's tree and the paid module commit its gitlink
// records there, or an empty commit when that tree has no gitlink.
func gitlinkAt(repo, tag string) (ref, commit string, err error) {
	for _, candidate := range []string{"refs/tags/" + tag, "refs/tags/snapshot/" + tag} {
		if exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", candidate).Run() != nil {
			continue
		}
		out, err := exec.Command("git", "-C", repo, "ls-tree", candidate, paidModule).Output()
		if err != nil {
			return "", "", fmt.Errorf("read the gitlink at %s: %w", candidate, err)
		}
		fields := strings.Fields(string(out))
		if len(fields) >= 3 && fields[1] == "commit" {
			return candidate, fields[2], nil
		}
		return candidate, "", nil
	}
	return "", "", fmt.Errorf("no %s or snapshot/%s tag in %s", tag, tag, repo)
}

// buildPreviousPaid builds the previous paid release's image the way the paid pipeline builds a
// release: the public tree at its tag, the paid module at the commit that tag's gitlink names, the
// module's superset go.mod over the root, and that tag's own Dockerfile with the paid build
// arguments. Nothing of the working tree goes into it.
func (h *harness) buildPreviousPaid(prev previousPaid) error {
	src := filepath.Join(h.work, "paid-v"+prev.Version)
	module := filepath.Join(src, paidModule)
	if err := os.MkdirAll(module, 0o750); err != nil {
		return err
	}
	public := filepath.Join(h.work, "paid-v"+prev.Version+"-public.tar")
	if _, err := h.run("git", "-C", h.repo, "archive", "--format=tar", "-o", public,
		prev.Ref); err != nil {
		return err
	}
	if _, err := h.run("tar", "-xf", public, "-C", src); err != nil {
		return err
	}
	private := filepath.Join(h.work, "paid-v"+prev.Version+"-module.tar")
	if _, err := h.run("git", "-C", filepath.Join(h.repo, paidModule), "archive", "--format=tar",
		"-o", private, prev.Commit); err != nil {
		return err
	}
	if _, err := h.run("tar", "-xf", private, "-C", module); err != nil {
		return err
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(module, "build", name))
		if err != nil {
			return fmt.Errorf("the paid module at %s has no superset %s: %w", prev.Commit, name, err)
		}
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			return err
		}
	}
	tags, main := buildTarget(true)
	if out, err := h.run("docker", "build", "-t", prev.Image,
		"--build-arg", "BUILD_TAGS="+tags, "--build-arg", "MAIN="+main,
		"--build-arg", "VERSION="+prev.Version, src); err != nil {
		return fmt.Errorf("build the previous paid image %s: %w\n%s", prev.Image, err, out)
	}
	return nil
}
