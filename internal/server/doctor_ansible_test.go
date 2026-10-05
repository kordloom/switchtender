package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
)

// TestDoctorReportsAnsibleCore pins what doctor says about the server's Ansible: the version and
// the tested releases always, a warning when the version is outside them, and, when Ansible is
// missing, a warning for each inventory whose definition needs it, with the one-line install. A
// smart inventory over static documents needs nothing, since the native engine resolves it.
func TestDoctorReportsAnsibleCore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want          error
		Version       string
		WantInstalled bool
		WantInRange   bool
		WantFindings  []string
	}{{ // Test 0: A tested release is reported with no finding.
		Version: "2.18.1", WantInstalled: true, WantInRange: true,
	}, { // Test 1: An untested release is a warning naming the tested ones.
		Version: "2.14.3", WantInstalled: true,
		WantFindings: []string{"install:ansible-core 2.14.3 is outside the releases"},
	}, { // Test 2: Missing Ansible is a warning on each inventory that needs it, and no other.
		Want: roundhouse.ErrAnsibleMissing,
		WantFindings: []string{"inventory:Needs Ansible because it is a constructed inventory",
			"inventory:Needs Ansible because it is an inventory plugin configuration"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			invs := inventory.NewMemStore()
			for _, inv := range []*inventory.Inventory{
				{ID: "inv_static", Name: "static", Content: "[web]\nweb1\n"},
				{ID: "inv_cloud", Name: "cloud", Content: "plugin: amazon.aws.aws_ec2\n"},
				{ID: "inv_smart", Name: "smart", Kind: inventory.KindSmart, HostFilter: "name=web1"},
				{ID: "inv_built", Name: "built", Kind: inventory.KindConstructed,
					InputIDs: []string{"inv_static"}},
			} {
				if err := invs.Save(ctx, inv); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			ansible := func(context.Context) (string, error) { return test.Version, test.Want }
			rec := httptest.NewRecorder()
			doctorHandler(nil, nil, nil, invs, nil, nil, nil, ansible, zap.NewNop()).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
			var report doctorReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			if report.Ansible == nil {
				t.Fatal("doctor reported nothing about Ansible")
			}
			a := report.Ansible
			if a.Installed != test.WantInstalled || a.InRange != test.WantInRange ||
				a.Version != test.Version {
				t.Errorf("ansible = %+v, want installed %v, in range %v, version %q", report.Ansible,
					test.WantInstalled, test.WantInRange, test.Version)
			}
			if diff := cmp.Diff(inventory.TestedAnsibleCore, report.Ansible.Tested); diff != "" {
				t.Errorf("tested releases mismatch (-want +got):\n%s", diff)
			}
			var got []string
			for _, f := range report.Findings {
				got = append(got, f.ObjectType+":"+f.Problem)
			}
			if len(got) != len(test.WantFindings) {
				t.Fatalf("findings = %q, want %d starting %q", got, len(test.WantFindings),
					test.WantFindings)
			}
			for i, want := range test.WantFindings {
				if !strings.HasPrefix(got[i], want) {
					t.Errorf("finding %d = %q, want it to start %q", i, got[i], want)
				}
				if errors.Is(test.Want, roundhouse.ErrAnsibleMissing) &&
					!strings.Contains(got[i], inventory.AnsibleInstallHint) {
					t.Errorf("finding %d does not give the install line: %q", i, got[i])
				}
			}
		})
	}
}
