package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestATokenSpanningLinesIsRefused covers the guard that keeps a token credential from becoming
// several environment variables.
//
// A token is exposed to the run as one variable. A container run writes the entries in spec.Env into
// an env file one line each, so a value with a newline in the middle stops being one value: a token
// whose stored form ends in "\nLD_PRELOAD=/tmp/evil.so" sets LD_PRELOAD for the whole run, on every
// host it touches. Values arrive from a secret source's stdout, where a stray newline is ordinary and
// not something an operator would notice.
//
// The guard existed with nothing executing it, so removing it left the suite green.
func TestATokenSpanningLinesIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Secret string
		// WantRefused is false for the shapes that must still be accepted, so the guard cannot be
		// widened into rejecting ordinary tokens.
		WantRefused bool
		// WantEnv is the single variable an accepted token must produce.
		WantEnv string
	}{
		{
			Name:        "a newline smuggling a second variable",
			Secret:      "t0ken\nLD_PRELOAD=/tmp/evil.so",
			WantRefused: true,
		},
		{
			Name:        "a carriage return smuggling a second variable",
			Secret:      "t0ken\rLD_PRELOAD=/tmp/evil.so",
			WantRefused: true,
		},
		{
			Name:        "a blank line before a second variable",
			Secret:      "t0ken\n\nPATH=/tmp/bin",
			WantRefused: true,
		},
		{
			Name:    "an ordinary token with a trailing newline from a secret source",
			Secret:  "t0ken\n",
			WantEnv: credential.TokenEnvVar + "=t0ken",
		},
		{
			Name:    "an ordinary token with trailing CRLF",
			Secret:  "t0ken\r\n",
			WantEnv: credential.TokenEnvVar + "=t0ken",
		},
		{
			Name:    "a plain token",
			Secret:  "t0ken",
			WantEnv: credential.TokenEnvVar + "=t0ken",
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal(test.Secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			store := credential.NewMemStore()
			if err := store.Save(context.Background(), &credential.Credential{
				ID: "cred_tok", Name: "api", Kind: credential.KindToken, Secret: sealed,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			d := &Dispatcher{credentials: store, sealer: sealer}
			spec := &roundhouse.Spec{}
			cleanup, secrets, err := d.materializeCredentials(context.Background(),
				&run.Run{ID: "run_tok", CredentialIDs: []string{"cred_tok"}}, spec)
			defer cleanup()

			if !test.WantRefused {
				if err != nil {
					t.Fatalf("materializeCredentials() error = %v, want the token accepted", err)
				}
				var found bool
				for _, e := range spec.Env {
					if e == test.WantEnv {
						found = true
					}
				}
				if !found {
					t.Errorf("spec.Env = %v, want %q", spec.Env, test.WantEnv)
				}
				return
			}

			if err == nil {
				t.Fatalf("materializeCredentials() accepted a token spanning lines; spec.Env = %v",
					spec.Env)
			}
			if !strings.Contains(err.Error(), "spans") {
				t.Errorf("error = %v, want it to name the token spanning more than one line", err)
			}
			// The refusal must not have already written the smuggled variable into the spec, or
			// refusing is only a message and the run still carries it.
			for _, e := range spec.Env {
				if strings.Contains(e, "LD_PRELOAD") || strings.Contains(e, "PATH=") {
					t.Errorf("spec.Env = %v, want no variable from the refused token", spec.Env)
				}
			}
			// The secret still has to be masked. A refused credential was opened, so its plaintext
			// reached this process and can land in an error or a log line.
			var masked bool
			for _, s := range secrets {
				if strings.Contains(s, "t0ken") {
					masked = true
				}
			}
			if !masked {
				t.Errorf("secrets = %v, want the opened token registered for masking even though "+
					"the credential was refused", secrets)
			}
			// The error message itself must not carry the secret.
			if strings.Contains(err.Error(), "t0ken") {
				t.Errorf("error = %v, want it to name the credential without quoting the token", err)
			}
		})
	}
}
