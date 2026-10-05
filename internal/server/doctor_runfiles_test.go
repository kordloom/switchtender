package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/runfiles"
)

// TestDoctorWarnsWhereRunFilesReachDisk pins the doctor's run-files findings: a warning for the
// temporary directory fallback, worded for whether it is memory-backed, a warning for any root on
// persistent disk, and silence for a memory-backed runtime directory or a server nobody told.
func TestDoctorWarnsWhereRunFilesReachDisk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// State is what the server knows about its root, nil for nothing.
		State *runFilesState
		// WantWords are what the one warning must say, empty for no finding.
		WantWords []string
	}{{ // Test 0: The temporary directory on disk, the case the root order exists to avoid.
		State: &runFilesState{source: runfiles.SourceTemp, report: runfiles.Report{
			Root: "/tmp/switchtender-runfiles-998", Filesystem: "ext4"}},
		WantWords: []string{"temporary directory", "ext4", "persistent disk", "--runfiles-dir"},
	}, { // Test 1: The temporary directory on tmpfs still survives the service.
		State: &runFilesState{source: runfiles.SourceTemp, report: runfiles.Report{
			Root: "/tmp/switchtender-runfiles-998", Filesystem: "tmpfs", MemoryBacked: true}},
		WantWords: []string{"temporary directory", "memory-backed here", "systemd unit"},
	}, { // Test 2: An explicit root on disk is still disk.
		State: &runFilesState{source: runfiles.SourceFlag, report: runfiles.Report{
			Root: "/srv/runfiles", Filesystem: "xfs"}},
		WantWords: []string{"/srv/runfiles", "xfs", "persistent disk"},
	}, { // Test 3: The systemd runtime directory is what the doctor wants to see.
		State: &runFilesState{source: runfiles.SourceSystemd, report: runfiles.Report{
			Root: "/run/switchtender/runfiles", Filesystem: "tmpfs", MemoryBacked: true}},
	}, { // Test 4: A private XDG runtime directory is fine too.
		State: &runFilesState{source: runfiles.SourceXDG, report: runfiles.Report{
			Root: "/run/user/998/switchtender-runfiles", Filesystem: "tmpfs", MemoryBacked: true}},
	}, { // Test 5: A server that was never told reports nothing rather than guessing.
		State: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			doctorHandler(nil, nil, nil, nil, nil, func() bool { return true }, test.State, nil,
				zap.NewNop()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
			var report doctorReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			var got []doctorFinding
			for _, f := range report.Findings {
				if f.ObjectID == "runfiles" {
					got = append(got, f)
				}
			}
			if len(test.WantWords) == 0 {
				if diff := cmp.Diff([]doctorFinding(nil), got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("findings (-want +got):\n%s", diff)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want one run-files warning", got)
			}
			f := got[0]
			if f.Severity != "warning" || f.ObjectType != "install" || f.FixPath != "/ui/docs/run-files" {
				t.Errorf("finding = %+v, want an install warning pointing at the run files page", f)
			}
			for _, w := range test.WantWords {
				if !strings.Contains(f.Problem, w) {
					t.Errorf("finding %q does not say %q", f.Problem, w)
				}
			}
		})
	}
}
