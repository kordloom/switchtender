package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestImportedFileInjectorIsRemovedOnEveryEnding is scenario two. AWX exports a custom credential
// type whose injector writes a file and hands its path to the play. The play has to read the file
// the type renders from the credential's fields, and the file has to be gone when the run ends,
// whichever way it ends: success, failure, or cancellation partway through.
func TestImportedFileInjectorIsRemovedOnEveryEnding(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a")
	token := "svc-token-" + randomHex(t, 12)
	in.fillCredential(s, "svc-token", map[string]string{
		"endpoint": "https://svc.example.invalid", "token": token,
	})
	credID := in.lookup(s, "credentials", "svc-token")
	sum := sha256.Sum256([]byte("endpoint=https://svc.example.invalid\ntoken=" + token))
	wantDigest := hex.EncodeToString(sum[:])

	tests := []struct {
		// Mode is what the play is told to do after reading the file.
		Mode string
		// WantStatus is how the run ends.
		WantStatus string
	}{
		{Mode: "ok", WantStatus: "succeeded"},
		{Mode: "fail", WantStatus: "failed"},
		{Mode: "wait", WantStatus: "canceled"},
	}
	var ids []string
	for _, test := range tests {
		marker := "token-" + test.Mode
		rec := in.launched(s, "operator", "use service token", map[string]any{
			"extra_vars": map[string]any{"mode": test.Mode, "marker_name": marker},
		})
		ids = append(ids, rec.ID)
		if test.Mode == "wait" {
			in.waitMarker(marker, "web1")
			in.must(s, "operator", "POST", "/v1/runs/"+rec.ID+"/cancel", nil, 200, 202)
		}
		done := in.waitDone(s, rec.ID)
		if done.Status != test.WantStatus {
			t.Fatalf("the %s run = %s, want %s: %s", test.Mode, done.Status, test.WantStatus,
				describe(done.Raw))
		}
		lines := strings.Split(in.waitMarker(marker, "web1"), "\n")
		if len(lines) < 2 {
			t.Fatalf("the %s run's marker is incomplete: %q", test.Mode, lines)
		}
		path, digest := lines[0], lines[1]
		if digest != wantDigest {
			t.Errorf("the %s run's play read a file whose digest is %s, want the rendered "+
				"injector's %s", test.Mode, digest, wantDigest)
		}
		if !strings.HasPrefix(path, in.runFiles+string(filepath.Separator)) {
			t.Errorf("the %s run's credential file %s is outside the run directory root %s",
				test.Mode, path, in.runFiles)
		}
		requireGone(t, "the "+test.Mode+" run's credential file", path, filepath.Dir(path))
		if dirs := in.runFileDirs(); len(dirs) != 0 {
			t.Errorf("run directories left after the %s run: %v", test.Mode, dirs)
		}
	}

	ev := in.checkEvidence(s, ids...)
	for i, test := range tests {
		requireRecord(t, ev.Receipts[ids[i]], recordWant{
			Status: test.WantStatus, Launcher: "operator-laptop", OnBehalfOf: "operator",
			Playbook: "token.yml", CredentialIDs: []string{credID},
		})
		if diff := cmp.Diff([]string{"web1"}, outcomeHosts(ev.Receipts[ids[i]].outcome(t))); diff != "" &&
			test.Mode != "wait" {
			t.Errorf("hosts the %s run's receipt records (-want +got):\n%s", test.Mode, diff)
		}
	}
}
