package roundhouse

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestConfigDoesNotReachAHostRun checks that SwitchTender's own configuration is absent from the
// environment of a run built the way production builds one.
//
// The whole process environment used to be captured once and handed to every host-mode child. The
// server reads its master encryption key and salt from there, so an operator allowed to submit a
// one-line shell run could print them, and that pair opens every stored credential and any backup.
// Submitting a run needs only the operator role, and the key was never registered with the masker,
// so it was not redacted on the way into the stored log either.
func TestConfigDoesNotReachAHostRun(t *testing.T) {
	const key, salt = "s3cret-master-passphrase", "stable-deployment-salt"
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", key)
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", salt)
	t.Setenv("SWITCHTENDER_WORKER_TOKEN", "relay-token")

	// Built through the same constructor the server uses, so this reflects the shipped wiring
	// rather than a runner assembled for the test.
	router := newToolRouter(false, "", "", false, ContainerLimits{})
	var out bytes.Buffer
	if _, err := router.bash.Run(context.Background(), Spec{
		Command: "echo KEY=$SWITCHTENDER_ENCRYPTION_KEY SALT=$SWITCHTENDER_ENCRYPTION_SALT " +
			"TOKEN=$SWITCHTENDER_WORKER_TOKEN",
		Dir: os.TempDir(),
	}, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := out.String()
	for _, secret := range []string{key, salt, "relay-token"} {
		if strings.Contains(got, secret) {
			t.Errorf("a run printed %q, so anyone who may submit one can read it: %q",
				secret, strings.TrimSpace(got))
		}
	}
	// The credentials the server reads under names it did not choose are stripped too. A deployment
	// resolving secrets from Vault or AWS Secrets Manager sets these, they carry no SwitchTender
	// prefix, and the masker never held them, so a one-line shell run printed the token that opens
	// every secret the server can reach into a stored log.
	t.Setenv("VAULT_TOKEN", "hvs.probe-vault-token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "probe-aws-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "probe-aws-session-token")
	t.Setenv("AWS_ACCESS_KEY_ID", "probe-aws-access-key-id")
	ambient := newToolRouter(false, "", "", false, ContainerLimits{})
	var ambientOut bytes.Buffer
	if _, err := ambient.bash.Run(context.Background(), Spec{
		Command: "echo V=$VAULT_TOKEN S=$AWS_SECRET_ACCESS_KEY T=$AWS_SESSION_TOKEN " +
			"I=$AWS_ACCESS_KEY_ID",
		Dir: os.TempDir(),
	}, &ambientOut); err != nil {
		t.Fatalf("Run() ambient error = %v", err)
	}
	for _, secret := range []string{
		"hvs.probe-vault-token", "probe-aws-secret-key", "probe-aws-session-token",
		"probe-aws-access-key-id",
	} {
		if strings.Contains(ambientOut.String(), secret) {
			t.Errorf("a run printed the server's %q, so anyone who may submit one holds the "+
				"credential the secret store is reached with: %q",
				secret, strings.TrimSpace(ambientOut.String()))
		}
	}

	// The host's own environment still reaches the run, since a playbook is often meant to use it.
	if !strings.Contains(os.Getenv("PATH"), "/") {
		t.Skip("no PATH to check inheritance against")
	}
	var inherited bytes.Buffer
	if _, err := router.bash.Run(context.Background(), Spec{
		Command: "echo PATH=$PATH", Dir: os.TempDir(),
	}, &inherited); err != nil {
		t.Fatalf("Run() inherited error = %v", err)
	}
	if !strings.Contains(inherited.String(), "/") {
		t.Error("the run inherited no PATH, so stripping our config took the host environment too")
	}
}
