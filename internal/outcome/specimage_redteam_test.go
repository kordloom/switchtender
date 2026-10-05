package outcome_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestApprovedSpecBindingCoversTheExecutionImage covers a gap in "an approval releases exactly what
// was approved": the execution image is the container the tool, its credentials, and its output all
// live in, so it decides what actually runs as surely as the command does. A run with no image of
// its own takes its project's default image, resolved at execution time, so the image in force when
// the approver decided is not pinned to the run. The approved-spec binding the executor re-checks is
// what is meant to refuse a spec that changed after approval, but it is taken over a record that
// excludes the image entirely. Two runs that differ only in the container they execute in therefore
// share one binding and one digest, so after an approver releases a run, changing its project's
// image, or the server default, swaps the container the approved work runs in with nothing in the
// gate, the binding, or the digest able to tell.
//
// This test holds the binding and the digest to covering the image. It fails today because both
// ignore it, and passes once the image is part of what an approval binds.
func TestApprovedSpecBindingCoversTheExecutionImage(t *testing.T) {
	t.Parallel()

	// Two Ansible runs identical in every field that executes, down to the playbook and inventory,
	// differing only in the image they run inside.
	approved := &run.Run{
		ID: "r", Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "hosts.ini",
		Image: "registry.example.com/trusted:v1",
	}
	swapped := &run.Run{
		ID: "r", Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "hosts.ini",
		Image: "registry.example.com/attacker:v1",
	}

	ba, err := outcome.SpecBinding(approved)
	if err != nil {
		t.Fatalf("SpecBinding(approved) error = %v", err)
	}
	bs, err := outcome.SpecBinding(swapped)
	if err != nil {
		t.Fatalf("SpecBinding(swapped) error = %v", err)
	}
	if ba == bs {
		t.Errorf("the approved and the image-swapped run share the binding %s, so the executor's "+
			"approved-spec check cannot tell that a run released to run in %q now runs in %q", ba,
			approved.Image, swapped.Image)
	}

	da, err := outcome.SpecDigest(approved)
	if err != nil {
		t.Fatalf("SpecDigest(approved) error = %v", err)
	}
	ds, err := outcome.SpecDigest(swapped)
	if err != nil {
		t.Fatalf("SpecDigest(swapped) error = %v", err)
	}
	if da == ds {
		t.Errorf("the approved and the image-swapped run share the spec digest %s, so a receipt "+
			"discloses no sign the approved container was swapped for another", da)
	}
}
