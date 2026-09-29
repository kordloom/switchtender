package importer

import (
	"strings"
	"testing"
	"time"
)

// TestAHostileGroupNameCannotBecomeAnIniSectionModifier is the guard on the one character class
// safeININame missed. An INI section named "all:vars" is not a group: it assigns variables to
// every host in the inventory, and "web:children" splices members into another group. An export
// is attacker-controlled input the moment somebody imports a file they were sent, so a group name
// carrying a colon has to be dropped and named, never written through.
func TestAHostileGroupNameCannotBecomeAnIniSectionModifier(t *testing.T) {
	t.Parallel()
	const export = `[
	 {"name":"web01.prod","chef_environment":"production",
	  "run_list":["role[all:vars]","role[web:children]","role[base]"]}
	]`
	plan, err := FromChef([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromChef: %v", err)
	}
	content := plan.Inventories[0].Content

	// Test 0: Neither modifier section exists in the rendered inventory.
	for _, hostile := range []string{"[all:vars]", "[web:children]"} {
		if strings.Contains(content, hostile) {
			t.Errorf("the inventory carries %q, an injected section modifier:\n%s", hostile, content)
		}
	}

	// Test 1: The legitimate group still renders, so the defense is a filter rather than a panic.
	if !strings.Contains(content, "[base]") {
		t.Errorf("the ordinary group was lost along with the hostile ones:\n%s", content)
	}

	// Test 2: The drop is named, because a silently vanished group is the other failure mode.
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "all:vars") {
		t.Errorf("the dropped group was not named in a warning:\n%s", joined)
	}
}
