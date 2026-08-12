package dispatch

import (
	"context"
	"io"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestFinalizePersistsFieldsResolvedMidRun pins that everything an executor learns while the run is
// under way survives the terminal write.
//
// The terminal write is a narrow update of the columns a finish owns, which is what keeps the stored
// run and the committed outcome digest from disagreeing. That narrowness is also a trap: three
// fields are resolved after the last whole-run save and before the finish. Image was carried across
// and its two siblings were not, so a project-backed run silently lost its commit provenance, which
// the dossier renders, and its pull credential, which is one of the grantable objects the run's own
// authorization is built from. Neither loss was visible anywhere.
func TestFinalizePersistsFieldsResolvedMidRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil)
	defer d.Close()

	r := &run.Run{ID: "run_midrun", Playbook: "site.yml", Status: run.StatusRunning}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// What resolveProject and the executor learn while the run is under way.
	r.CommitSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	r.PullCredentialID = "cred_pull"
	r.Image = "registry.example.com/ee@sha256:abc"

	d.finalize(r, run.StatusSucceeded, nil, "")

	stored, err := store.Get(ctx, "run_midrun")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	for _, f := range []struct {
		Name string
		Got  string
		Want string
	}{
		{Name: "commit_sha", Got: stored.CommitSHA, Want: r.CommitSHA},
		{Name: "pull_credential_id", Got: stored.PullCredentialID, Want: r.PullCredentialID},
		{Name: "image", Got: stored.Image, Want: r.Image},
	} {
		if f.Got != f.Want {
			t.Errorf("stored %s = %q, want %q; it was resolved mid-run and the terminal write "+
				"dropped it", f.Name, f.Got, f.Want)
		}
	}
}
