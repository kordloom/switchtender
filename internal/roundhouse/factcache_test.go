package roundhouse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestFactCacheReachesAnsibleOnTheHost pins that a spec carrying a fact cache directory points
// ansible-playbook's jsonfile cache plugin at it, the settings AWX uses, and that a spec without
// one leaves the cache plugin alone so a project's own ansible.cfg still decides.
func TestFactCacheReachesAnsibleOnTheHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "ansible-playbook")
	writeStub(t, stub, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\nenv\n")
	cache := filepath.Join(dir, "facts")

	var withCache, without bytes.Buffer
	r := NewAnsibleRunner(WithBinary(stub))
	if _, err := r.Run(context.Background(), Spec{Playbook: "site.yml", FactCacheDir: cache},
		&withCache); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := r.Run(context.Background(), Spec{Playbook: "site.yml"}, &without); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, want := range []string{"ANSIBLE_CACHE_PLUGIN=jsonfile",
		"ANSIBLE_CACHE_PLUGIN_CONNECTION=" + cache, "ANSIBLE_CACHE_PLUGIN_TIMEOUT=0"} {
		if !strings.Contains(withCache.String(), want) {
			t.Errorf("environment with a fact cache lacks %q:\n%s", want, withCache.String())
		}
	}
	if strings.Contains(without.String(), "ANSIBLE_CACHE_PLUGIN=") {
		t.Errorf("a run without a fact cache set the cache plugin:\n%s", without.String())
	}
}

// TestFactCacheReachesAnsibleInAContainer pins the container side: the cache directory is mounted
// writable, since the play writes the facts it gathers there, and the environment file the
// container sources points the plugin at it.
func TestFactCacheReachesAnsibleInAContainer(t *testing.T) {
	t.Parallel()
	cache := filepath.Join(t.TempDir(), "facts")
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{
		Playbook: "/checkout/site.yml", Dir: "/checkout", Image: "quay.io/ansible/creator-ee:latest",
		Tool: run.ToolAnsible, FactCacheDir: cache,
	}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "fc-test", "")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-v "+cache+":"+cache+" ") {
		t.Errorf("runArgs() = %q, want the fact cache mounted writable", joined)
	}
	if strings.Contains(joined, cache+":"+cache+":ro") {
		t.Errorf("runArgs() = %q mounts the fact cache read-only, so the play cannot write facts", joined)
	}
	envFile, remove, err := c.writeEnvFile(spec, nil)
	if err != nil {
		t.Fatalf("writeEnvFile() error = %v", err)
	}
	defer remove()
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(data), "ANSIBLE_CACHE_PLUGIN_CONNECTION") ||
		!strings.Contains(string(data), cache) {
		t.Errorf("env file does not point the cache plugin at the directory:\n%s", data)
	}
}
