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

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
)

// TestDoctorReportsAnsibleCore pins what doctor says about the server's Ansible: the version and
// the tested releases always, a warning when the version is outside them, and, when Ansible is
// missing, a warning for each inventory whose definition needs it, with the one-line install. A
// smart inventory over static documents needs nothing, since the native engine resolves it.
func TestDoctorReportsAnsibleCore(t *testing.T) {
	t.Parallel()
	managed := ansibleruntime.Commands{Source: ansibleruntime.SourceManaged, Release: "2.21.4",
		Dir: "/srv/st/ansible/2.21.4/bin", Root: "/srv/st/ansible"}
	tests := []struct {
		Want          error
		Version       string
		Commands      ansibleruntime.Commands
		WantInstalled bool
		WantInRange   bool
		WantFindings  []string
		WantInstall   string
	}{{ // Test 0: A tested release is reported with no finding.
		Version: "2.18.1", WantInstalled: true, WantInRange: true,
	}, { // Test 1: An untested release is a warning naming the tested ones.
		Version: "2.14.3", WantInstalled: true,
		WantFindings: []string{"install:ansible-core 2.14.3 is outside the releases"},
	}, { // Test 2: Missing Ansible is a warning on each inventory that needs it, and no other.
		Want: roundhouse.ErrAnsibleMissing,
		WantFindings: []string{"inventory:Needs Ansible because it is a constructed inventory",
			"inventory:Needs Ansible because it is an inventory plugin configuration"},
	}, { // Test 3: The managed runtime is reported with where it is.
		Version: "2.21.4", Commands: managed, WantInstalled: true, WantInRange: true,
	}, { // Test 4: The install line names the runtime directory the server looks in.
		Want:     roundhouse.ErrAnsibleMissing,
		Commands: ansibleruntime.Commands{Source: ansibleruntime.SourcePath, Root: "/srv/st/ansible"},
		WantFindings: []string{"inventory:Needs Ansible because it is a constructed inventory",
			"inventory:Needs Ansible because it is an inventory plugin configuration"},
		WantInstall: "switchtender ansible install --dir /srv/st/ansible, or on the system with: " +
			"pipx install ansible-core",
	}, { // Test 5: A managed runtime in use that cannot be used is a broken finding of its own.
		Want: roundhouse.ErrAnsibleMissing,
		Commands: ansibleruntime.Commands{Source: ansibleruntime.SourceManaged,
			Root: "/srv/st/ansible", Problem: "/srv/st/ansible is writable by accounts other than " +
				"its owner"},
		WantFindings: []string{"install:The Ansible this server is set to run cannot be used",
			"inventory:Needs Ansible because it is a constructed inventory",
			"inventory:Needs Ansible because it is an inventory plugin configuration"},
		WantInstall: "switchtender ansible install --dir /srv/st/ansible, or on the system with: " +
			"pipx install ansible-core",
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
			cmds := test.Commands
			if cmds.Source == "" {
				cmds.Source = ansibleruntime.SourcePath
			}
			ansible := func(context.Context) (string, ansibleruntime.Commands, error) {
				return test.Version, cmds, test.Want
			}
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
			where := struct{ Source, Dir, RuntimeDir, Problem string }{a.Source, a.Dir,
				a.RuntimeDir, a.Problem}
			wantWhere := struct{ Source, Dir, RuntimeDir, Problem string }{cmds.Source, cmds.Dir,
				cmds.Root, cmds.Problem}
			if diff := cmp.Diff(wantWhere, where); diff != "" {
				t.Errorf("where Ansible comes from mismatch (-want +got):\n%s", diff)
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
				install := inventory.AnsibleInstallHint
				if test.WantInstall != "" {
					install = test.WantInstall
				}
				if errors.Is(test.Want, roundhouse.ErrAnsibleMissing) &&
					!strings.Contains(got[i], install) {
					t.Errorf("finding %d does not give the install line %q: %q", i, install, got[i])
				}
			}
		})
	}
}
