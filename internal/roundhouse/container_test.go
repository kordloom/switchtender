package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRegistryHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
	}{
		{In: "alpine", Want: ""},                                   // Test 0: Bare image.
		{In: "library/alpine:3.19", Want: ""},                      // Test 1: Docker Hub namespace.
		{In: "quay.io/ansible/creator-ee:latest", Want: "quay.io"}, // Test 2: Registry with a dot.
		{In: "localhost:5000/team/img", Want: "localhost:5000"},    // Test 3: Local registry with a port.
		{In: "ghcr.io/org/img@sha256:abc", Want: "ghcr.io"},        // Test 4: Digest reference.
		{In: "localhost/img", Want: "localhost"},                   // Test 5: Bare localhost.
	}
	for i, test := range tests {
		if got := registryHost(test.In); got != test.Want {
			t.Errorf("test %d: registryHost(%q) = %q, want %q", i, test.In, got, test.Want)
		}
	}
}

func TestRunArgs(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{},
		ContainerLimits{Memory: "1g", CPUs: "2", PidsLimit: 512, Network: "bridge"})
	spec := Spec{
		Playbook:  "/checkout/site.yml",
		Inventory: "/checkout/hosts.ini",
		Dir:       "/checkout",
		Image:     "quay.io/ansible/creator-ee:latest",
		Limit:     "web01",
	}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "ym-test", "/tmp/env")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --rm --name ym-test",
		"--memory 1g",
		"--cpus 2",
		"--pids-limit 512",
		"--network bridge",
		"-w /checkout",
		// The env is mounted and sourced, never passed with --env-file, so its values never enter the
		// container's inspectable environment.
		"-v /tmp/env:/tmp/env:ro",
		"-v /checkout:/checkout:ro",
		`sh -c . "$1"; shift; exec "$@" sh /tmp/env ansible-playbook`,
		"-i /checkout/hosts.ini --limit web01 -- /checkout/site.yml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("runArgs() = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--env-file") {
		t.Errorf("runArgs() passes --env-file, which would expose secrets to docker inspect: %v", args)
	}
	// The image must precede the shell wrapper that sources the env and execs the tool.
	if idx := slices.Index(args, spec.Image); idx == -1 || args[idx+1] != "sh" {
		t.Errorf("image not positioned before the command: %v", args)
	}
}

func TestRunArgsRefusesSensitiveMounts(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	tests := []struct {
		Name string
		Spec Spec
	}{
		{"root dir", Spec{Playbook: "/site.yml", Dir: "/", Image: "alpine"}},                                     // Test 0.
		{"etc dir", Spec{Playbook: "/etc/site.yml", Dir: "/etc", Image: "alpine"}},                               // Test 1.
		{"docker socket", Spec{Playbook: "/checkout/s.yml", Inventory: "/var/run/docker.sock", Image: "alpine"}}, // Test 2.
		// The socket's directory is the same escape as the socket. /var is refused but subpaths are
		// deliberately allowed, so /var/run needs naming on its own or a container gets the host.
		{"var run dir", Spec{Playbook: "/checkout/s.yml", Dir: "/var/run", Image: "alpine"}}, // Test 3.
		{"run dir", Spec{Playbook: "/checkout/s.yml", Dir: "/run", Image: "alpine"}},         // Test 4.
		{"podman socket", Spec{Playbook: "/checkout/s.yml", // Test 5.
			Inventory: "/run/podman/podman.sock", Image: "alpine"}},
		{"docker lib", Spec{Playbook: "/checkout/s.yml", Dir: "/var/lib/docker", Image: "alpine"}}, // Test 6.
	}
	for i, test := range tests {
		plan, cleanup, err := buildContainerPlan(test.Spec)
		if err != nil {
			cleanup()
			t.Fatalf("test %d (%s): buildContainerPlan() error = %v", i, test.Name, err)
		}
		_, err = c.runArgs(test.Spec, plan, "ym-test", "/tmp/env")
		cleanup()
		if !errors.Is(err, ErrForbiddenMount) {
			t.Errorf("test %d (%s): runArgs() error = %v, want ErrForbiddenMount", i, test.Name, err)
		}
	}
}

func TestValidateImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want error
	}{
		{In: "alpine", Want: nil},                            // Test 0: Bare image.
		{In: "quay.io/ansible/creator-ee:latest", Want: nil}, // Test 1: Registry, repo, tag.
		{In: "ghcr.io/org/img@sha256:abc123", Want: nil},     // Test 2: Digest reference.
		{In: "", Want: ErrNoImage},                           // Test 3: Empty.
		{In: "-rm", Want: ErrBadImage},                       // Test 4: Leading dash reads as a flag.
		{In: "alpine; rm -rf /", Want: ErrBadImage},          // Test 5: Shell metacharacters.
		{In: "../../etc", Want: ErrBadImage},                 // Test 6: Path traversal.
		{In: "img latest", Want: ErrBadImage},                // Test 7: Whitespace.
	}
	for i, test := range tests {
		if err := validateImage(test.In); !errors.Is(err, test.Want) {
			t.Errorf("test %d: validateImage(%q) error = %v, want %v", i, test.In, err, test.Want)
		}
	}
}

func TestContainerRunRejectsBadImage(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	res, err := c.Run(context.Background(), Spec{Playbook: "p.yml", Image: "-badflag"}, io.Discard)
	if !errors.Is(err, ErrBadImage) {
		t.Errorf("Run() error = %v, want ErrBadImage", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestSelectRunnerRefusesContainerWhenDisabled(t *testing.T) {
	t.Parallel()
	r := NewSelectiveRunner(false, "docker", "missing", false, DefaultContainerLimits())
	res, err := r.Run(context.Background(), Spec{Playbook: "p.yml", Image: "alpine"}, io.Discard)
	if !errors.Is(err, ErrContainerDisabled) {
		t.Errorf("Run() error = %v, want ErrContainerDisabled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestRunArgsPullPolicy(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"always", "missing", "never"} {
		c := newContainerRunner("docker", policy, false, nil, &pluginCache{}, DefaultContainerLimits())
		spec := Spec{Playbook: "/checkout/site.yml", Dir: "/checkout", Image: "quay.io/ansible/creator-ee:latest"}
		plan, cleanup, err := buildContainerPlan(spec)
		if err != nil {
			t.Fatalf("policy %q: buildContainerPlan() error = %v", policy, err)
		}
		args, err := c.runArgs(spec, plan, "ym-test", "/tmp/env")
		cleanup()
		if err != nil {
			t.Fatalf("policy %q: runArgs() error = %v", policy, err)
		}
		joined := strings.Join(args, " ")
		if want := "--pull " + policy; !strings.Contains(joined, want) {
			t.Errorf("policy %q: runArgs() = %q, missing %q", policy, joined, want)
		}
		// The pull policy sits right after the container name, before the resource caps.
		if idx := slices.Index(args, "--pull"); idx == -1 || args[idx-1] != "ym-test" || args[idx+1] != policy {
			t.Errorf("policy %q: --pull not positioned after the name: %v", policy, args)
		}
	}
}

func TestIsDigestPinned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want bool
	}{
		{In: "quay.io/x/y@sha256:abc123", Want: true},       // Test 0: Digest reference.
		{In: "quay.io/x/y:latest", Want: false},             // Test 1: Tag-only reference.
		{In: "alpine", Want: false},                         // Test 2: Bare image.
		{In: "ghcr.io/org/img@sha256:deadbeef", Want: true}, // Test 3: Registry digest.
	}
	for i, test := range tests {
		if got := isDigestPinned(test.In); got != test.Want {
			t.Errorf("test %d: isDigestPinned(%q) = %v, want %v", i, test.In, got, test.Want)
		}
	}
}

func TestValidateRunImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In            string
		Want          error
		RequireDigest bool
	}{
		{In: "quay.io/x/y:latest", RequireDigest: false, Want: nil},             // Test 0: Tag-only, pinning off.
		{In: "quay.io/x/y@sha256:abc123", RequireDigest: false, Want: nil},      // Test 1: Digest, pinning off.
		{In: "quay.io/x/y:latest", RequireDigest: true, Want: ErrUnpinnedImage}, // Test 2: Tag-only, pinning on.
		{In: "quay.io/x/y@sha256:abc123", RequireDigest: true, Want: nil},       // Test 3: Digest, pinning on.
	}
	for i, test := range tests {
		c := newContainerRunner("docker", "missing", test.RequireDigest, nil, &pluginCache{},
			DefaultContainerLimits())
		if err := c.validateRunImage(test.In); !errors.Is(err, test.Want) {
			t.Errorf("test %d: validateRunImage(%q) error = %v, want %v", i, test.In, err, test.Want)
		}
	}
}

func TestContainerRunRejectsUnpinnedImage(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", true, nil, &pluginCache{}, DefaultContainerLimits())
	res, err := c.Run(context.Background(), Spec{Playbook: "p.yml", Image: "quay.io/x/y:latest"}, io.Discard)
	if !errors.Is(err, ErrUnpinnedImage) {
		t.Errorf("Run() error = %v, want ErrUnpinnedImage", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestRegistryLoginIsScopedToTheRun proves a private-image credential does not outlive the run that
// used it, and does not authenticate a later run of a different project.
//
// The runtime writes a login into its config directory, and that directory used to be the executor's
// own. One project's credential therefore stayed on disk after its run and served every later pull,
// so a project with no credential could pull from a registry it was never granted. The login now
// lands in a per-run directory that is removed with the run.
func TestRegistryLoginIsScopedToTheRun(t *testing.T) {
	t.Parallel()
	dir, cleanup, err := newRuntimeConfigDir()
	if err != nil {
		t.Fatalf("newRuntimeConfigDir() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	// Another user on the executor must not read a credential out of it.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %v, want 0700", perm)
	}
	// A second run gets its own directory, so one run's login cannot serve another.
	other, cleanupOther, err := newRuntimeConfigDir()
	if err != nil {
		t.Fatalf("second newRuntimeConfigDir() error = %v", err)
	}
	if other == dir {
		t.Error("two runs share a registry config directory, so one run's login serves the other")
	}
	cleanupOther()

	// Writing a credential there and cleaning up removes it from disk.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the registry config survived the run, err = %v", err)
	}
}

// TestCredentialFilesAreMountedForEveryTool proves a credential injection's file reaches the
// container, whatever tool the run uses.
//
// An injection writes the file on the host and points an environment variable at it. The variable
// crosses into the container unchanged, so without the mount the tool is handed a path that is not
// there: a GCP service account or a kubeconfig silently fails to apply and the run looks like a
// broken credential rather than a missing mount.
func TestCredentialFilesAreMountedForEveryTool(t *testing.T) {
	t.Parallel()
	credFile := filepath.Join(t.TempDir(), "gcp.json")
	if err := os.WriteFile(credFile, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	tools := []struct {
		Tool string
		Spec Spec
	}{
		{"ansible", Spec{Tool: "ansible", Playbook: "/checkout/site.yml", Dir: "/checkout",
			Image: "alpine", CredentialFiles: []string{credFile}}},
		{"bash", Spec{Tool: "bash", Command: "echo hi", Dir: "/checkout",
			Image: "alpine", CredentialFiles: []string{credFile}}},
		{"python", Spec{Tool: "python", Command: "print(1)", Dir: "/checkout",
			Image: "alpine", CredentialFiles: []string{credFile}}},
		{"terraform", Spec{Tool: "terraform", Command: "/checkout", Dir: "/checkout",
			Image: "alpine", CredentialFiles: []string{credFile}}},
	}
	for _, tc := range tools {
		plan, cleanup, err := buildContainerPlan(tc.Spec)
		if err != nil {
			cleanup()
			t.Fatalf("%s: buildContainerPlan() error = %v", tc.Tool, err)
		}
		var mounted bool
		for _, m := range plan.mounts {
			if m.path == credFile {
				mounted = true
			}
		}
		cleanup()
		if !mounted {
			t.Errorf("%s: the credential file is not mounted, so the variable pointing at it names "+
				"a path that does not exist in the container", tc.Tool)
		}
	}
}

// TestRunArgsRunsAsHostUser checks the container runs as the host executor's own uid:gid, so the
// private 0600/0700 files it owns (callback config, credential files, events sidecar) are readable
// and writable by the identically-uid'd in-container process. Without a matching --user the sidecar
// and callback silently fail and a run's events are lost.
func TestRunArgsRunsAsHostUser(t *testing.T) {
	t.Parallel()
	if os.Getuid() < 0 {
		t.Skip("uid mapping is a unix concern")
	}
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Playbook: "/work/site.yml", Dir: "/work", Image: "alpine"}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "ym-test", "/tmp/env")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	idx := slices.Index(args, "--user")
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("runArgs() = %q, missing a --user flag", args)
	}
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if diff := cmp.Diff(want, args[idx+1]); diff != "" {
		t.Errorf("--user value mismatch (-want +got):\n%s", diff)
	}
	// The flag must precede the image so it configures the run, not the tool.
	if img := slices.Index(args, "alpine"); img != -1 && idx > img {
		t.Errorf("--user at %d comes after the image at %d, want it as a run flag", idx, img)
	}
}
