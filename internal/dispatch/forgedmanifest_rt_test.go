package dispatch

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/kordloom/switchtender/internal/run"
)

// rtForgedCheckouts serves one committed tree as a project's checkout, standing in for the PR-head
// commit a review trigger reads. The tree is whatever the pull request author committed, including a
// .terraform directory they force-added.
type rtForgedCheckouts struct {
	fsys fs.FS
}

// ReadCommit serves the committed tree for any project and commit.
func (c rtForgedCheckouts) ReadCommit(_, _ string, fn func(fs.FS, string) error) error {
	return fn(c.fsys, "0123456789abcdef0123456789abcdef01234567")
}

// TestReviewScanDistrustsACommittedModuleManifest pins that the plan gate does not classify a
// Terraform plan as change-free on the strength of a .terraform/modules/modules.json the pull
// request author committed.
//
// The review flow scans the configuration from the PR head commit (scanConfiguration ->
// tfscan.Scan over the commit's fs). tfscan trusts .terraform/modules/modules.json as the record
// of what a trusted `terraform init` already downloaded, which is sound only in the
// server-working-directory flow where the server's own init wrote it. In the commit flow that file
// is attacker-committed: a registry or git module that the gate must refuse (its real content is not
// in the checkout and `init` downloads it only after the gate decides) is made to look downloaded by
// committing a manifest that points at a clean planted copy. The scan reads the clean copy and
// reports change_free, so the plan runs; the run's own `terraform init` then overwrites the module
// with the real remote one, whose `data "external"` executes a program during plan with the
// template's credentials from an unmerged branch, which is exactly what refusing an external data
// source and an unreadable module exists to prevent.
//
// The safe outcome is that a non-local module backed only by a committed manifest is unreadable, so
// the plan is incomplete and the gate holds it, the same answer Test 12 gives when no manifest is
// present at all.
func TestReviewScanDistrustsACommittedModuleManifest(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{
		"main.tf": &fstest.MapFile{Data: []byte(
			"module \"vpc\" {\n  source  = \"terraform-aws-modules/vpc/aws\"\n  version = \"~> 5.0\"\n}\n")},
		".terraform/modules/modules.json": &fstest.MapFile{Data: []byte(
			`{"Modules":[{"Key":"","Source":"","Dir":"."},` +
				`{"Key":"vpc","Source":"registry.terraform.io/terraform-aws-modules/vpc/aws",` +
				`"Version":"5.1.2","Dir":".terraform/modules/vpc"}]}`)},
		// The planted clean copy. The real registry module, downloaded by init at plan time,
		// declares a data "external" that runs a program.
		".terraform/modules/vpc/main.tf": &fstest.MapFile{Data: []byte("output \"o\" {\n  value = 1\n}\n")},
	}
	r := &run.Run{
		ProjectID: "proj_rt", Tool: run.ToolTerraform, DryRun: true, Command: ".",
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}
	got := scanConfiguration(r, rtForgedCheckouts{fsys: tree})
	if got.Classification == run.DryRunChangeFree {
		t.Fatalf("the gate classified a plan change_free on a pull-request-committed module manifest; "+
			"a registry module backed only by a committed manifest must be unreadable (incomplete), "+
			"got classification %q findings %q unread %q",
			got.Classification, got.Findings, got.Unread)
	}
}
