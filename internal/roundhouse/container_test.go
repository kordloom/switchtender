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

func TestDockerArgs(t *testing.T) {
	t.Parallel()
	c := newContainerRunner(nil, &pluginCache{})
	spec := Spec{
		Playbook:  "/checkout/site.yml",
		Inventory: "/checkout/hosts.ini",
		Dir:       "/checkout",
		Image:     "quay.io/ansible/creator-ee:latest",
		Limit:     "web01",
	}
	args, err := c.dockerArgs(spec, "ym-test", "/tmp/env")
	if err != nil {
		t.Fatalf("dockerArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --rm --name ym-test",
		"-w /checkout",
		"--env-file /tmp/env",
		"-v /checkout:/checkout:ro",
		"quay.io/ansible/creator-ee:latest ansible-playbook",
		"-i /checkout/hosts.ini --limit web01 /checkout/site.yml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dockerArgs() = %q, missing %q", joined, want)
		}
	}
	// The image must precede the ansible-playbook command, not appear as a mount.
	if idx := slices.Index(args, spec.Image); idx == -1 || args[idx+1] != "ansible-playbook" {
		t.Errorf("image not positioned before the command: %v", args)
	}
}

func TestSelectRunnerRefusesContainerWhenDisabled(t *testing.T) {
	t.Parallel()
	r := NewSelectiveRunner(false)
	res, err := r.Run(context.Background(), Spec{Playbook: "p.yml", Image: "alpine"}, io.Discard)
	if !errors.Is(err, ErrContainerDisabled) {
		t.Errorf("Run() error = %v, want ErrContainerDisabled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}
