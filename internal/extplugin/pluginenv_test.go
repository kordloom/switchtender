package extplugin

import (
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
