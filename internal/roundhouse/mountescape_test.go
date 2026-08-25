package roundhouse

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestASensitiveSubpathCannotBeMounted pins that the mount guard refuses the children of a
// sensitive directory, not only the directory itself.
//
// The blocklist matched exact paths, and a run names the file it wants rather than the directory
// above it, so it stopped the mount nobody was trying to make. /etc was refused while /etc/shadow
// was allowed, /root while /root/.ssh was allowed. A run is not required to name a project, and a
// project-less run's playbook and inventory paths reach the container plan unvalidated, so the
// paths mounted here are the caller's.
func TestASensitiveSubpathCannotBeMounted(t *testing.T) {
	t.Parallel()
	refused := []string{
		"/etc/shadow", "/etc/passwd", "/etc/ssh/ssh_host_rsa_key", "/etc/kubernetes/admin.conf",
		"/root/.ssh", "/root/.ssh/id_rsa", "/proc/self/environ", "/sys/kernel",
		"/dev/mem", "/boot/vmlinuz", "/var/run/secrets", "/run/podman",
		"/var/lib/docker/volumes", "/home/ops/.ssh/id_ed25519", "/Users/ops/.aws/credentials",
		"/srv/checkout/.git/../../../etc/shadow",
		// The directory itself stays refused; the tree check must not have replaced that.
		"/etc", "/root", "/", "/var", "/home",
	}
	for i, path := range refused {
		t.Run(fmt.Sprintf("test %d %s", i, path), func(t *testing.T) {
			t.Parallel()
			if err := checkMountPath(path); !errors.Is(err, ErrForbiddenMount) {
				t.Errorf("checkMountPath(%q) = %v, want ErrForbiddenMount", path, err)
			}
		})
	}
}

// TestAnOrdinaryPathStillMounts pins that the tree refusal did not take the paths a run actually
// needs with it. A checkout and the state directory live under a user's home or under /var, which
// is why those cannot be refused as whole trees.
func TestAnOrdinaryPathStillMounts(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"/srv/checkout", "/home/ops/checkout/site.yml", "/Users/ops/switchtender/state",
		"/var/folders/xy/T/switchtender-123/vars.json", "/var/lib/switchtender/data",
		"/tmp/switchtender-py-1.py", "/opt/projects/infra",
		// Ends in a sensitive name but is not one, so the component match must not catch it.
		"/srv/checkout/myssh", "/srv/notetc/file",
	}
	for i, path := range allowed {
		t.Run(fmt.Sprintf("test %d %s", i, path), func(t *testing.T) {
			t.Parallel()
			if err := checkMountPath(path); err != nil {
				t.Errorf("checkMountPath(%q) = %v, want nil", path, err)
			}
		})
	}
}

// TestCallerSuppliedRunPathsCannotMountHostSecrets is the same refusal reached the way a request
// reaches it, through the container plan a run produces, rather than through the guard alone.
func TestCallerSuppliedRunPathsCannotMountHostSecrets(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	tests := []struct {
		Name string
		Spec Spec
	}{{ // Test 0: The inventory is mounted at its own path, so it names any file on the host.
		Name: "inventory names a host secret",
		Spec: Spec{Playbook: "/srv/checkout/s.yml", Inventory: "/etc/shadow", Image: "alpine"},
	}, { // Test 1: The playbook's directory is mounted, so it names any directory on the host.
		Name: "playbook sits in the host's key directory",
		Spec: Spec{Playbook: "/root/.ssh/s.yml", Dir: "/srv/checkout", Image: "alpine"},
	}, { // Test 2: A home-directory credential store, which no tree refusal can cover.
		Name: "inventory names a home credential store",
		Spec: Spec{Playbook: "/srv/checkout/s.yml", Dir: "/srv/checkout",
			Inventory: "/home/ops/.aws/credentials", Image: "alpine"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, cleanup, err := buildContainerPlan(test.Spec)
			if err != nil {
				cleanup()
				t.Fatalf("buildContainerPlan() error = %v", err)
			}
			args, err := c.runArgs(test.Spec, plan, "st-test", "/tmp/env")
			cleanup()
			if !errors.Is(err, ErrForbiddenMount) {
				t.Fatalf("runArgs() error = %v, want ErrForbiddenMount.\nargs: %s",
					err, strings.Join(args, " "))
			}
			if slices.ContainsFunc(args, func(a string) bool {
				return strings.Contains(a, "shadow") || strings.Contains(a, ".ssh") ||
					strings.Contains(a, ".aws")
			}) {
				t.Errorf("a refused path still reached the argument list: %s", strings.Join(args, " "))
			}
		})
	}
}
