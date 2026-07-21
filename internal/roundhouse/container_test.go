package roundhouse

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
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
		"--env-file /tmp/env",
		"-v /checkout:/checkout:ro",
		"quay.io/ansible/creator-ee:latest ansible-playbook",
		"-i /checkout/hosts.ini --limit web01 /checkout/site.yml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("runArgs() = %q, missing %q", joined, want)
		}
	}
	// The image must precede the ansible-playbook command, not appear as a mount.
	if idx := slices.Index(args, spec.Image); idx == -1 || args[idx+1] != "ansible-playbook" {
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
