package dispatch

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// TestAMintedLoginIsStillLiveWhenTheRunPullsItsImage covers the lifetime of a secret a dynamic
// engine mints, which is a different question from whether the secret is correct.
//
// A dynamic source hands back a value and a lease, and the value stops working the moment the lease
// is handed back. The registry login is read by the container runner when it pulls, which happens
// well after the function that resolved it returned, so releasing the lease on the way out of that
// function killed the credential before anything used it: every containerized run on an install
// keeping registry logins in Vault or its peers failed to pull an image it was entitled to.
//
// Every other credential on this path already works the other way, handing the caller one cleanup to
// defer beside the run. This asserts the property rather than the shape: at the moment the runner
// executes, the lease has not been revoked, and once the run is over it has.
func TestAMintedLoginIsStillLiveWhenTheRunPullsItsImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var revoked atomic.Bool
	var liveAtPull atomic.Bool
	var pulled atomic.Bool

	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			// The runner stands in for the pull: it reads the login the way the container runner
			// does, and records whether the lease behind it is still live at that instant.
			if spec.RegistryPassword != "" {
				pulled.Store(true)
				liveAtPull.Store(!revoked.Load())
			}
			return roundhouse.Result{ExitCode: 0}, nil
		})

	store := run.NewMemStore()
	creds, sealer := leasedRegistryCredential(t, &revoked)
	d := New(store, runner, nil, WithNoJanitor(), WithCredentials(creds, sealer))
	defer d.Close()

	created, err := d.Submit(ctx, "", "inv", run.WithTool(run.ToolBash), run.WithCommand("id"),
		run.WithImage("registry.example/exec:1", "pull_1"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, func() bool {
		got, gerr := store.Get(ctx, created.ID)
		return gerr == nil && got.Status.Terminal()
	})

	if !pulled.Load() {
		t.Fatal("the runner never saw a registry login, so this test is not exercising the pull")
	}
	if !liveAtPull.Load() {
		t.Error("the minted registry login was already revoked when the run pulled its image, so " +
			"an install keeping registry logins in a dynamic engine cannot pull at all")
	}
	waitFor(t, func() bool { return revoked.Load() })
}

// leasedRegistryCredential builds a registry credential backed by a dynamic engine, so resolving it
// mints the login and produces a lease whose revocation this test can observe.
func leasedRegistryCredential(t *testing.T, revoked *atomic.Bool) (credential.Store, *credential.Sealer) {
	t.Helper()
	kind := "test-mint-" + t.Name()
	secretsource.RegisterDynamic(kind, func(_ context.Context, _ string) (string, *secretsource.Lease, error) {
		return "registry-user\nregistry-pass\n", secretsource.NewLease(kind, func(context.Context) error {
			revoked.Store(true)
			return nil
		}), nil
	})

	sealer := credential.NewSealer("pass", "salt")
	// What is sealed for a sourced credential is the source's configuration, not the secret.
	sealed, err := sealer.Seal("{}")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "pull_1", Name: "ghcr", Kind: credential.KindRegistry, Source: kind, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() credential error = %v", err)
	}
	return creds, sealer
}
