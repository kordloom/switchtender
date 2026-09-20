package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// chefExport is a node dump in the shape Chef Infra Server stores: two production nodes sharing a
// role, one staging node, run lists mixing roles and recipes, and ohai attributes far wider than
// what an inventory should carry.
const chefExport = `[
 {"name":"web01.prod","chef_environment":"production",
  "run_list":["role[base]","role[webserver]","recipe[nginx::default]"],
  "automatic":{"fqdn":"web01.prod","ipaddress":"10.0.1.5","platform":"ubuntu",
   "platform_version":"22.04","os":"linux","packages":{"nginx":{"version":"1.24"}}}},
 {"name":"db01.prod","chef_environment":"production","run_list":["role[base]","role[database]"],
  "automatic":{"fqdn":"db01.prod","ipaddress":"10.0.2.9","platform":"rocky"}},
 {"name":"web01.stage","chef_environment":"staging","run_list":["role[webserver]"],
  "automatic":{"ipaddress":"10.1.1.5"}}
]`

// TestChefBringsTheFleetAndSaysWhatItLeftBehind covers the whole point of this importer: the fleet
// and its shape come across, and the thing that does not is named rather than discovered later.
func TestChefBringsTheFleetAndSaysWhatItLeftBehind(t *testing.T) {
	t.Parallel()
	plan, err := FromChef([]byte(chefExport), time.Now())
	if err != nil {
		t.Fatalf("FromChef: %v", err)
	}
	if len(plan.Inventories) != 1 {
		t.Fatalf("got %d inventories, want 1", len(plan.Inventories))
	}
	content := plan.Inventories[0].Content

	// Test 0: Every node is a host, and each role and environment is a group it belongs to.
	for _, want := range []string{"web01.prod", "db01.prod", "web01.stage",
		"[base]", "[webserver]", "[database]", "[production]", "[staging]"} {
		if !strings.Contains(content, want) {
			t.Errorf("inventory is missing %q:\n%s", want, content)
		}
	}

	// Test 1: A host carries the address a play connects on, or the inventory is a list of names
	// that resolve through DNS a Chef estate has no reason to maintain.
	if !strings.Contains(content, "ansible_host=10.0.1.5") {
		t.Errorf("no ansible_host was derived from the node's ipaddress:\n%s", content)
	}

	// Test 2: The wide attributes stay out. An ohai dump on a host line is a file nobody can read.
	if strings.Contains(content, "packages") || strings.Contains(content, "1.24") {
		t.Errorf("ohai package data was copied onto a host line:\n%s", content)
	}

	// Test 3: The cookbooks are named as not coming across, in the report an operator acts on.
	joined := strings.Join(plan.Report().LeftOut, "\n")
	if !strings.Contains(joined, "recipes are not imported") {
		t.Errorf("the report does not say cookbooks stay behind:\n%s", joined)
	}
	if !strings.Contains(joined, "nginx::default") {
		t.Errorf("the recipes in the run lists were not named:\n%s", joined)
	}
}

// TestChefReadsTheShapesTheToolingActuallyEmits exists because which shape somebody has depends on
// how they dumped it, and refusing two of three reads as the importer not supporting Chef.
func TestChefReadsTheShapesTheToolingActuallyEmits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Doc is the export as the tooling produced it.
		Doc string
		// WantHost is a host the inventory must carry.
		WantHost string
	}{{ // Test 0: An array of node documents, which is a bulk dump.
		Doc:      chefExport,
		WantHost: "web01.prod",
	}, { // Test 1: One node document, which is knife node show for a single machine.
		Doc:      `{"name":"solo.prod","chef_environment":"production","run_list":["role[base]"]}`,
		WantHost: "solo.prod",
	}, { // Test 2: An object keyed by node name, where the key carries the name.
		Doc: `{"keyed.prod":{"chef_environment":"production","run_list":["role[web]"]},
		       "other.prod":{"chef_environment":"production","run_list":["role[web]"]}}`,
		WantHost: "keyed.prod",
	}}
	for testNum, test := range tests {
		t.Run(strings.Join([]string{"test", string(rune('0' + testNum))}, " "), func(t *testing.T) {
			t.Parallel()
			plan, err := FromChef([]byte(test.Doc), time.Now())
			if err != nil {
				t.Fatalf("FromChef: %v", err)
			}
			if len(plan.Inventories) != 1 {
				t.Fatalf("got %d inventories, want 1", len(plan.Inventories))
			}
			if !strings.Contains(plan.Inventories[0].Content, test.WantHost) {
				t.Errorf("inventory is missing %q:\n%s", test.WantHost, plan.Inventories[0].Content)
			}
		})
	}
}

// TestChefRefusesADocumentThatIsNotAChefExport keeps the importer from turning arbitrary JSON into
// an inventory of nothing, which reads as a successful import of an empty fleet.
func TestChefRefusesADocumentThatIsNotAChefExport(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{`{"totally":"unrelated"}`, `[]`, `{}`} {
		if _, err := FromChef([]byte(doc), time.Now()); err == nil {
			t.Errorf("FromChef(%s) succeeded, want a refusal", doc)
		}
	}
}

// TestPuppetLeavesOutWhatPuppetStoppedManaging is the one judgment this importer makes about the
// fleet rather than about the format.
//
// A deactivated node is still in the query result and is not part of the estate any more. Importing
// it puts a machine in the inventory that nothing manages, and a play targeting everything reaches
// for it, which is a failure at run time rather than at import time.
func TestPuppetLeavesOutWhatPuppetStoppedManaging(t *testing.T) {
	t.Parallel()
	const export = `[
	 {"certname":"live.prod","catalog_environment":"production","latest_report_status":"changed"},
	 {"certname":"gone.prod","catalog_environment":"production","deactivated":"2026-08-01T00:00:00Z"},
	 {"certname":"aged.prod","catalog_environment":"production","expired":"2026-07-01T00:00:00Z"}
	]`
	plan, err := FromPuppet([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromPuppet: %v", err)
	}
	content := plan.Inventories[0].Content
	if !strings.Contains(content, "live.prod") {
		t.Errorf("the live node is missing:\n%s", content)
	}
	for _, gone := range []string{"gone.prod", "aged.prod"} {
		if strings.Contains(content, gone) {
			t.Errorf("%q is deactivated in PuppetDB and was imported anyway:\n%s", gone, content)
		}
	}
	if joined := strings.Join(plan.Report().LeftOut, "\n"); !strings.Contains(joined, "deactivated") {
		t.Errorf("nodes were dropped without the report saying so:\n%s", joined)
	}
}

// TestPuppetReadsNodesFactsAndAPlainList covers the three ways a fleet arrives, because which one
// somebody has depends on what they can reach: PuppetDB, or only the CA.
func TestPuppetReadsNodesFactsAndAPlainList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Doc is the export.
		Doc string
		// WantHost is a host the inventory must carry.
		WantHost string
		// WantVar is a host variable the inventory must carry, empty when none is expected.
		WantVar string
	}{{ // Test 0: A PuppetDB nodes query.
		Doc:      `[{"certname":"a.prod","catalog_environment":"production","latest_report_status":"changed"}]`,
		WantHost: "a.prod", WantVar: "puppet_last_run=changed",
	}, { // Test 1: A PuppetDB facts query, which names its nodes one fact row at a time.
		Doc: `[{"certname":"b.prod","name":"ipaddress","value":"10.0.0.2"},
		       {"certname":"b.prod","name":"osfamily","value":"RedHat"}]`,
		WantHost: "b.prod", WantVar: "ansible_host=10.0.0.2",
	}, { // Test 2: The plain certname list `puppet node list` prints.
		Doc:      "c.prod\nd.prod\n\n# a comment\n",
		WantHost: "c.prod",
	}}
	for testNum, test := range tests {
		t.Run(strings.Join([]string{"test", string(rune('0' + testNum))}, " "), func(t *testing.T) {
			t.Parallel()
			plan, err := FromPuppet([]byte(test.Doc), time.Now())
			if err != nil {
				t.Fatalf("FromPuppet: %v", err)
			}
			content := plan.Inventories[0].Content
			if !strings.Contains(content, test.WantHost) {
				t.Errorf("inventory is missing %q:\n%s", test.WantHost, content)
			}
			if test.WantVar != "" && !strings.Contains(content, test.WantVar) {
				t.Errorf("inventory is missing %q:\n%s", test.WantVar, content)
			}
		})
	}
}

// TestPuppetSaysWhenAFleetArrivedWithNoFacts guards the quiet failure. A nodes query with no facts
// produces an inventory of names with no address, which looks complete and reaches nothing.
func TestPuppetSaysWhenAFleetArrivedWithNoFacts(t *testing.T) {
	t.Parallel()
	plan, err := FromPuppet([]byte(`[{"certname":"a.prod","catalog_environment":"production"}]`),
		time.Now())
	if err != nil {
		t.Fatalf("FromPuppet: %v", err)
	}
	all := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(all, "no facts") {
		t.Errorf("a factless import did not say the hosts carry no address:\n%s", all)
	}
}

// TestPuppetRefusesADocumentThatNamesNoNode keeps arbitrary JSON from importing as an empty fleet.
func TestPuppetRefusesADocumentThatNamesNoNode(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{`[]`, `[{"unrelated":1}]`, "   "} {
		if _, err := FromPuppet([]byte(doc), time.Now()); err == nil {
			t.Errorf("FromPuppet(%q) succeeded, want a refusal", doc)
		}
	}
}

// TestEveryChefFormReportsWhatItDoesNotRead pins the unread scan to all three shapes the tooling
// emits. It was true only of the array form: a keyed or single-node dump was compared against the
// array shape, matched nothing, and so was never scanned, which dropped fields with no warning in
// exactly the forms where the summary still claimed nothing had been.
func TestEveryChefFormReportsWhatItDoesNotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Doc is the export in one of the three shapes, each carrying policy_group, which this
		// importer does not read.
		Doc string
	}{{ // Test 0: An array of node documents.
		Doc: `[{"name":"a.prod","chef_environment":"production","run_list":["role[web]"],"policy_group":"x"}]`,
	}, { // Test 1: An object keyed by node name.
		Doc: `{"a.prod":{"chef_environment":"production","run_list":["role[web]"],"policy_group":"x"}}`,
	}, { // Test 2: One node document.
		Doc: `{"name":"a.prod","chef_environment":"production","run_list":["role[web]"],"policy_group":"x"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromChef([]byte(test.Doc), time.Now())
			if err != nil {
				t.Fatalf("FromChef: %v", err)
			}
			joined := strings.Join(plan.Warnings, "\n")
			if !strings.Contains(joined, "policy_group") {
				t.Errorf("policy_group was dropped without a warning in this form:\n%s", joined)
			}
		})
	}
}
