package extplugin

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestPluginEnvWithholdsInstallSecrets proves a plugin subprocess does not inherit the environment
// this server reads its own secrets from.
//
// The plugin library appends the host environment to every subprocess it launches unless told not
// to, so a drop-in binary received the deployment encryption key and salt, the worker token, and
// every configured provider secret without asking. The key seals every stored credential, so that
// one variable is enough to read them all. The allowlist is deny-by-default on purpose: the deny
// list grows every time this server learns to read another secret from the environment, and a
// missed entry hands that secret to every plugin on the machine.
func TestPluginEnvWithholdsInstallSecrets(t *testing.T) {
	withheld := []string{
		"SWITCHTENDER_ENCRYPTION_KEY", "SWITCHTENDER_ENCRYPTION_SALT", "SWITCHTENDER_AUDIT_KEY",
		"SWITCHTENDER_WORKER_TOKEN", "SWITCHTENDER_AI_KEY", "SWITCHTENDER_SMTP_PASSWORD",
		"SWITCHTENDER_LDAP_PASSWORD", "SWITCHTENDER_OIDC_CLIENT_SECRET",
		"SWITCHTENDER_ADMIN_PASSWORD", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	}
	for _, name := range withheld {
		t.Setenv(name, "secret-value-"+name)
	}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SWITCHTENDER_PLUGIN_NOTIFY_FILE", "/tmp/notify")

	env := pluginEnv()
	joined := strings.Join(env, "\n")
	for _, name := range withheld {
		if strings.Contains(joined, name+"=") {
			t.Errorf("a plugin would receive %s, which this server reads its own secrets from", name)
		}
		if strings.Contains(joined, "secret-value-"+name) {
			t.Errorf("the value of %s reached the plugin environment", name)
		}
	}
	// What a process genuinely needs still passes, or plugins simply stop working.
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Errorf("PATH was withheld, so a plugin cannot find anything: %v", env)
	}
	// The operator's deliberate, namespaced configuration passes.
	if !slices.Contains(env, "SWITCHTENDER_PLUGIN_NOTIFY_FILE=/tmp/notify") {
		t.Errorf("a namespaced plugin variable was withheld: %v", env)
	}
}

// TestPluginEnvNamesAreExactOutsideWindows pins how a variable name is matched against the
// allowlist. Every name was upper-cased before the lookup, which is right only on Windows, where
// Path and PATH are one variable. On Linux and macOS they are two, so a server variable named path,
// home, or switchtender_plugin_x reached every plugin under a name the allowlist never granted.
// SYSTEMROOT and USERPROFILE passed everywhere too, though only Windows needs them.
func TestPluginEnvNamesAreExactOutsideWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		GOOS     string
		WantPass bool
	}{{ // Test 0: The exact name passes on Linux.
		Name: "PATH", GOOS: "linux", WantPass: true,
	}, { // Test 1: A differently cased name is a different variable on Linux.
		Name: "Path", GOOS: "linux",
	}, { // Test 2: It is the same variable on Windows.
		Name: "Path", GOOS: "windows", WantPass: true,
	}, { // Test 3: A lowercase home is not HOME on macOS.
		Name: "home", GOOS: "darwin",
	}, { // Test 4: SYSTEMROOT is withheld outside Windows.
		Name: "SYSTEMROOT", GOOS: "linux",
	}, { // Test 5: Windows gets it under the case it usually carries.
		Name: "SystemRoot", GOOS: "windows", WantPass: true,
	}, { // Test 6: USERPROFILE is withheld outside Windows.
		Name: "USERPROFILE", GOOS: "darwin",
	}, { // Test 7: Windows gets it.
		Name: "USERPROFILE", GOOS: "windows", WantPass: true,
	}, { // Test 8: The plugin prefix passes on Linux.
		Name: "SWITCHTENDER_PLUGIN_NOTIFY_FILE", GOOS: "linux", WantPass: true,
	}, { // Test 9: A lowercase prefix is another variable on Linux.
		Name: "switchtender_plugin_notify_file", GOOS: "linux",
	}, { // Test 10: On Windows the plugin reads it as the prefixed name, so it passes.
		Name: "switchtender_plugin_notify_file", GOOS: "windows", WantPass: true,
	}, { // Test 11: A server secret is withheld on Windows whatever its case.
		Name: "switchtender_encryption_key", GOOS: "windows",
	}, { // Test 12: The same secret is withheld on Linux.
		Name: "SWITCHTENDER_ENCRYPTION_KEY", GOOS: "linux",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := passesToPlugin(test.Name, test.GOOS); got != test.WantPass {
				t.Errorf("passesToPlugin(%q, %q) = %v, want %v", test.Name, test.GOOS, got, test.WantPass)
			}
		})
	}
}
