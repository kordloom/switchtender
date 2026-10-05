package dossier

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestDossierShowsTheResolvedHosts pins that the evidence for a run against a smart or constructed
// inventory names the machines it resolved to at launch. The inventory is evaluated rather than
// stored, so a dossier naming only its id would leave an auditor unable to say which hosts the
// change was aimed at.
func TestDossierShowsTheResolvedHosts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Resolution  *run.InventoryResolution
		WantContain []string
		WantAbsent  []string
	}{{ // Test 0: A composed run lists its kind, its inputs, and every host.
		Resolution: &run.InventoryResolution{
			Kind: "smart", Inputs: []string{"inv_web", "inv_db"}, Hosts: []string{"db1", "web1"},
		},
		WantContain: []string{"Hosts the inventory resolved to at launch",
			"smart inventory, 2 hosts at launch", "inv_web, inv_db", ">db1<", ">web1<"},
	}, { // Test 1: A run against an ordinary inventory carries no such section.
		Resolution: nil,
		WantAbsent: []string{"Hosts the inventory resolved to at launch", "Inventory resolved"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := &Input{
				Run: &run.Run{
					ID: "run_c", Playbook: "site.yml", InventoryID: "inv_smart",
					Status:              run.StatusSucceeded,
					InventoryResolution: test.Resolution,
				},
				ChainOK: true, GeneratedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			}
			doc, err := Render(in)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			for _, want := range test.WantContain {
				if !strings.Contains(string(doc), want) {
					t.Errorf("the dossier lacks %q", want)
				}
			}
			for _, absent := range test.WantAbsent {
				if strings.Contains(string(doc), absent) {
					t.Errorf("the dossier carries %q for a run with no composed inventory", absent)
				}
			}
		})
	}
}
