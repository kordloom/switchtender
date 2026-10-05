package migration

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestFailedRunLeavesNoCredentialTokenOrFactFiles is scenario ten. A run of an imported template
// carries a file-injected custom credential, a federated AWS credential, and the fact cache, and
// its play fails after the files are in place. Every one of them, the credential file, the identity
// token file, the fact cache directory, and the stored inventory's materialized file, has to sit
// under the run-files root, where a crash leaves it to the sweep, and be gone when the failed run
// ends.
func TestFailedRunLeavesNoCredentialTokenOrFactFiles(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a", "--federation-issuer", federationIssuer)
	in.fillCredential(s, "svc-token", map[string]string{
		"endpoint": "https://svc.example.invalid", "token": "svc-token-" + randomHex(t, 12),
	})
	svcID := in.lookup(s, "credentials", "svc-token")
	fedID := in.createFederated(s, "aws-cleanup")
	in.updateTemplate(s, "cleanup probe", func(tpl map[string]any) {
		tpl["credential_ids"] = []string{svcID, fedID}
	})

	rec := in.launched(s, "operator", "cleanup probe", nil)
	done := in.waitDone(s, rec.ID)
	if done.Status != "failed" {
		t.Fatalf("the probe run = %s, want failed: %s", done.Status, describe(done.Raw))
	}
	lines := strings.Split(strings.TrimSpace(in.marker("cleanup", "web1")), "\n")
	if len(lines) != 4 {
		t.Fatalf("the play recorded %d paths, want the credential file, the token file, the "+
			"fact cache directory, and the inventory file: %q", len(lines), lines)
	}
	names := []string{"credential file", "identity token file", "fact cache directory",
		"inventory file"}
	for i, p := range lines {
		if p == "" {
			t.Fatalf("the play saw no %s, so the run was not given one", names[i])
		}
		if !strings.HasPrefix(p, in.runFiles+string(filepath.Separator)) {
			t.Errorf("the %s %s is outside the run directory root %s", names[i], p, in.runFiles)
		}
		requireGone(t, "the failed run's "+names[i], p)
	}
	if dirs := in.runFileDirs(); len(dirs) != 0 {
		t.Errorf("run directories left after the failed run: %v", dirs)
	}

	ev := in.checkEvidence(s, rec.ID)
	want := []string{svcID, fedID}
	sort.Strings(want)
	got := stringsOf(ev.Receipts[rec.ID].outcome(t).Spec["credential_ids"])
	sort.Strings(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("credentials the failed run's receipt records (-want +got):\n%s", diff)
	}
	requireRecord(t, ev.Receipts[rec.ID], recordWant{
		Status: "failed", Launcher: "operator-laptop", OnBehalfOf: "operator",
		Playbook: "cleanup.yml", Hosts: []string{"web1"},
	})
}
