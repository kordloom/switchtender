package util

import (
	"os"
	"slices"
	"strings"
)

// configEnvPrefix is the prefix every SwitchTender configuration variable carries.
const configEnvPrefix = "SWITCHTENDER_"

// The server reads these from its own environment as credentials for reaching an external secret
// manager. They are named here, beside the filter that strips them, so the place that reads one and
// the place that withholds it from a run cannot drift apart: a new ambient credential added to a
// secret source has to be added to this list to be readable at all.
const (
	// EnvVaultToken authenticates the server to HashiCorp Vault.
	EnvVaultToken = "VAULT_TOKEN"
	// EnvAWSAccessKeyID, EnvAWSSecretAccessKey, and EnvAWSSessionToken authenticate the server to
	// AWS Secrets Manager.
	EnvAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	EnvAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	EnvAWSSessionToken    = "AWS_SESSION_TOKEN"
)

// ambientCredentialEnv lists the unprefixed variables that are the server's own credentials.
var ambientCredentialEnv = []string{
	EnvVaultToken, EnvAWSAccessKeyID, EnvAWSSecretAccessKey, EnvAWSSessionToken,
}

// RunEnviron returns the environment for a child process that runs somebody's playbook, script, or
// plan: the process environment with SwitchTender's own configuration and credentials removed.
//
// The server reads its master encryption key, its salt, its worker token, and every provider secret
// from the environment. Passing that environment through to a run handed all of it to whatever the
// run executes: an operator with permission to submit a one-line shell run could print the master
// key, and with the key and salt they could decrypt every stored credential and any backup. The run
// is the thing the key protects, so the key does not belong in it.
//
// Removing the whole prefix covers everything SwitchTender names for itself, and a secret added to
// the configuration later is stripped by having the prefix, which is the property a denylist lacks.
// It does not cover the credentials the server reads under names it did not choose: a deployment
// that resolves secrets from Vault or AWS Secrets Manager sets VAULT_TOKEN and the AWS keys, which
// carry no prefix and so reached every host run. Submitting a run needs only the operator role and
// no credential access, so one line of shell returned the token that opens every secret the server
// can reach, and the masker held only the run's own credentials so it was stored in the log
// verbatim. Those names are stripped here as well.
//
// Everything else outside the prefix is left alone, because a playbook is often meant to inherit the
// proxy and locale settings of the host it runs on.
func RunEnviron() []string {
	return filterRunEnv(os.Environ())
}

// SecretFetchEnviron returns the environment for a command whose job is to fetch a secret.
//
// This is the one child process that must keep the ambient credentials RunEnviron strips, because
// reaching the secret store is the whole point of running it: a fetch command authenticates to Vault
// with VAULT_TOKEN. It still loses SwitchTender's own configuration, so the operator who configures
// a command source cannot read the master key back out through it. Configuring one is admin-only for
// that reason.
func SecretFetchEnviron() []string {
	return FilterConfigEnv(os.Environ())
}

// FilterConfigEnv returns env without SwitchTender's own configuration variables.
func FilterConfigEnv(env []string) []string {
	return filterEnv(env, false)
}

// filterRunEnv returns env without SwitchTender's configuration or the credentials it reaches an
// external secret manager with.
func filterRunEnv(env []string) []string {
	return filterEnv(env, true)
}

// filterEnv drops SwitchTender's own configuration, and the ambient credentials too when asked.
func filterEnv(env []string, dropCredentials bool) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, configEnvPrefix) {
			continue
		}
		name, _, found := strings.Cut(kv, "=")
		if dropCredentials && found && slices.Contains(ambientCredentialEnv, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
