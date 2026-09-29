package secretsource

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestACommandSourceCannotReadTheServersOwnConfiguration covers the one secret-fetch path that runs
// operator-supplied shell.
//
// A command source exists so the real secret never lives in SwitchTender: the value is fetched from
// Vault or a cloud CLI at run time. The command ran with the server's whole environment, and the
// server reads its master encryption key and salt from the environment, so the command that fetches
// one credential could print the key that protects every credential. Anyone who can configure a
// command source, or edit one, could read the key, and with the key and salt decrypt every stored
// credential and any backup.
//
// Every tool runner already strips the configuration prefix before handing an environment to
// somebody's playbook. The secret-fetch command was the one subprocess that never did.
func TestACommandSourceCannotReadTheServersOwnConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command source runs its config through sh")
	}

	// The server's own configuration, as a serve process holds it while credentials are enabled.
	const keyValue = "MASTER_KEY_MUST_NOT_ESCAPE"
	const saltValue = "MASTER_SALT_MUST_NOT_ESCAPE"
	const workerValue = "WORKER_TOKEN_MUST_NOT_ESCAPE"
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", keyValue)
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", saltValue)
	t.Setenv("SWITCHTENDER_WORKER_TOKEN", workerValue)

	// A variable the fetch command legitimately needs: the credential it authenticates to the secret
	// store with is supplied the same way, outside our prefix, and has to survive.
	const vaultValue = "vault-token-the-fetch-needs"
	t.Setenv("VAULT_TOKEN", vaultValue)

	got, err := Resolve(context.Background(), KindCommand, "env")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	for _, secret := range []struct {
		// Name is the variable, for the failure message.
		Name string
		// Value is the value that must not reach the command.
		Value string
	}{
		{"SWITCHTENDER_ENCRYPTION_KEY", keyValue},
		{"SWITCHTENDER_ENCRYPTION_SALT", saltValue},
		{"SWITCHTENDER_WORKER_TOKEN", workerValue},
	} {
		if strings.Contains(got, secret.Value) {
			t.Errorf("a secret-fetch command read %s from the server's environment: with the key and "+
				"salt the operator who configured this source can decrypt every stored credential",
				secret.Name)
		}
	}

	// The environment the command does need is still there, so the scrub did not break the feature.
	if !strings.Contains(got, vaultValue) {
		t.Errorf("the fetch command lost VAULT_TOKEN, which is how it authenticates to the secret "+
			"store:\n%s", got)
	}
	if !strings.Contains(got, "PATH=") {
		t.Errorf("the fetch command ran with no PATH:\n%s", got)
	}
}

// TestResolveCommandKeepsTheHostEnvironmentItNeeds confirms the scrub is by prefix rather than by a
// list of names, so a configuration secret added later is withheld without anyone remembering to add
// it here.
func TestResolveCommandKeepsTheHostEnvironmentItNeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command source runs its config through sh")
	}
	t.Setenv("SWITCHTENDER_SOME_SECRET_ADDED_LATER", "invented-after-this-test-was-written")
	t.Setenv("AWS_REGION", "us-east-2")

	got, err := Resolve(context.Background(), KindCommand, "env")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if strings.Contains(got, "invented-after-this-test-was-written") {
		t.Error("a configuration variable nobody listed reached the fetch command")
	}
	if !strings.Contains(got, "us-east-2") {
		t.Error("the fetch command lost the host's cloud configuration")
	}
	if os.Getenv("SWITCHTENDER_SOME_SECRET_ADDED_LATER") == "" {
		t.Error("the test did not set the variable it is checking for")
	}
}
