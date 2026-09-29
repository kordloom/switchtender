package demo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// factsPlay is the shape of assets/facts.yml a test needs: the fleet map declaring what each host
// is, which the play publishes through the same facts channel a gather uses.
type factsPlay struct {
	// Vars holds the play's variables.
	Vars struct {
		// Fleet maps a hostname to that host's declared system facts.
		Fleet map[string]map[string]string `yaml:"fleet"`
	} `yaml:"vars"`
}

// hostsInInventory returns the hostnames an INI inventory declares, in the order they appear.
func hostsInInventory(t *testing.T, content string) []string {
	t.Helper()
	var hosts []string
	scan := bufio.NewScanner(strings.NewReader(content))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "#") {
			continue
		}
		hosts = append(hosts, strings.Fields(line)[0])
	}
	if err := scan.Err(); err != nil {
		t.Fatalf("scan inventory: %v", err)
	}
	return hosts
}

// inventoryHosts returns the hostnames assets/inv.ini declares, in the order they appear. It reads
// the embedded inventory rather than a copy, so a host added there is a host these tests see.
func inventoryHosts(t *testing.T) []string {
	t.Helper()
	body, err := assets.ReadFile("assets/inv.ini")
	if err != nil {
		t.Fatalf("read assets/inv.ini: %v", err)
	}
	hosts := hostsInInventory(t, string(body))
	if len(hosts) == 0 {
		t.Fatal("assets/inv.ini declares no hosts")
	}
	return hosts
}

// declaredFleet returns the per-host facts assets/facts.yml declares.
func declaredFleet(t *testing.T) map[string]map[string]string {
	t.Helper()
	body, err := assets.ReadFile("assets/facts.yml")
	if err != nil {
		t.Fatalf("read assets/facts.yml: %v", err)
	}
	var plays []factsPlay
	if err := yaml.Unmarshal(body, &plays); err != nil {
		t.Fatalf("parse assets/facts.yml: %v", err)
	}
	if len(plays) != 1 {
		t.Fatalf("assets/facts.yml holds %d plays, want one", len(plays))
	}
	if len(plays[0].Vars.Fleet) == 0 {
		t.Fatal("assets/facts.yml declares no fleet, so every host would report the demo server")
	}
	return plays[0].Vars.Fleet
}

// TestDeclaredFactsCoverEveryInventoryHost pins that the fleet the facts play describes is the
// fleet the inventory declares. A host in the inventory with nothing declared for it is skipped by
// the play and shows an empty facts panel; a host declared but not in the inventory is a row
// nobody ever sees.
func TestDeclaredFactsCoverEveryInventoryHost(t *testing.T) {
	t.Parallel()
	fleet := declaredFleet(t)

	declared := make([]string, 0, len(fleet))
	for host := range fleet {
		declared = append(declared, host)
	}
	sort.Strings(declared)
	want := slices.Clone(inventoryHosts(t))
	sort.Strings(want)
	if diff := cmp.Diff(want, declared, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the declared fleet does not match the inventory (-inventory +declared):\n%s", diff)
	}
}

// TestDeclaredFactsDifferBetweenHosts is the guard against the demo fleet collapsing back into one
// machine listed once per host.
//
// Every host in this inventory is the box serving the demo, so gathering facts reported the same
// distribution, kernel, memory, and processor count on all six rows, along with that box's own
// hostname and private address. A fleet page whose rows are identical reads as a mock-up, and a
// drift feature has nothing to distinguish. Each declared fact therefore has to take at least two
// values across the fleet, and no two hosts may be described identically.
func TestDeclaredFactsDifferBetweenHosts(t *testing.T) {
	t.Parallel()
	fleet := declaredFleet(t)
	if len(fleet) < 2 {
		t.Fatalf("the declared fleet holds %d hosts, too few to differ", len(fleet))
	}

	values := make(map[string][]string)
	for _, facts := range fleet {
		for key, value := range facts {
			if !slices.Contains(values[key], value) {
				values[key] = append(values[key], value)
			}
		}
	}
	for key, seen := range values {
		if len(seen) < 2 {
			t.Errorf("every host reports %s = %q, so that column reads as one machine repeated",
				key, seen[0])
		}
	}

	// The address is per host rather than per tier, so two hosts sharing one means a copied block
	// somebody forgot to finish editing.
	if got, want := len(values["ip"]), len(fleet); got != want {
		t.Errorf("the fleet declares %d addresses across %d hosts, want one each", got, want)
	}

	fingerprints := make(map[string]string, len(fleet))
	for host, facts := range fleet {
		keys := make([]string, 0, len(facts))
		for key := range facts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, key := range keys {
			fmt.Fprintf(&b, "%s=%s;", key, facts[key])
		}
		if other, clash := fingerprints[b.String()]; clash {
			t.Errorf("hosts %s and %s are described identically", other, host)
		}
		fingerprints[b.String()] = host
	}
}

// TestFleetHostsMatchTheDemoInventory pins that the configuration state written to disk covers the
// hosts the drift check runs against. A host in the inventory with no state directory fails the
// check on a missing destination rather than reporting drift, which reads on the drift page as the
// check being broken.
func TestFleetHostsMatchTheDemoInventory(t *testing.T) {
	t.Parallel()
	want := slices.Clone(inventoryHosts(t))
	sort.Strings(want)

	got := make([]string, 0, len(fleetHosts))
	for _, h := range fleetHosts {
		got = append(got, h.Name)
	}
	sort.Strings(got)
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("fleetHosts does not match the inventory (-inventory +fleetHosts):\n%s", diff)
	}
}

// TestFleetStateGivesTheDriftCheckSomethingToFind pins the drift spread host by host.
//
// The check compares each host's files against the desired ones, so a fleet where every host is
// stale in the same files reports the same count on every row, and a fleet where none is stale
// reports nothing at all. Both make the drift page look like a feature that does not work. The
// spread below is the one the rest of the seeded history tells: the database primary and the edge
// host are behind, the edge host by a whole release, and two web hosts are in sync.
func TestFleetStateGivesTheDriftCheckSomethingToFind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Host      string
		WantStale []string
	}{{ // Test 0: A host in sync, which is what makes a drifted one mean something.
		Host: "web01", WantStale: nil,
	}, { // Test 1: One file behind.
		Host: "web02", WantStale: []string{"app.conf"},
	}, { // Test 2: The second host in sync.
		Host: "web03", WantStale: nil,
	}, { // Test 3: The database primary, behind on its config and its package pins.
		Host: "db01", WantStale: []string{"app.conf", "packages.pin"},
	}, { // Test 4: Its replica, behind on package pins alone.
		Host: "db02", WantStale: []string{"packages.pin"},
	}, { // Test 5: The edge host, a whole release behind.
		Host: "edge01", WantStale: []string{"app.conf", "worker.service", "packages.pin"},
	}}

	byHost := make(map[string][]string, len(fleetHosts))
	for _, h := range fleetHosts {
		byHost[h.Name] = h.Stale
	}
	var inSync, drifted int
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantStale, byHost[test.Host], cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s drift profile mismatch (-want +got):\n%s", test.Host, diff)
			}
		})
		if len(test.WantStale) == 0 {
			inSync++
		} else {
			drifted++
		}
	}
	if inSync == 0 || drifted == 0 {
		t.Errorf("the fleet holds %d hosts in sync and %d drifted, want both", inSync, drifted)
	}

	counts := make(map[int]bool)
	for _, h := range fleetHosts {
		counts[len(h.Stale)] = true
	}
	if len(counts) < 3 {
		t.Errorf("the fleet drifts by %d distinct amounts, so the drift page reads as a flat wall",
			len(counts))
	}
}

// TestWriteFleetStateWritesWhatTheCheckCompares pins that the state materialize writes really does
// differ from the desired configuration exactly where the table says it does. The drift page's
// numbers come from a byte comparison of these files, so an in-sync host whose file is not byte for
// byte the desired one reports drift that does not exist, and a stale file that happens to match
// reports none where there should be some.
func TestWriteFleetStateWritesWhatTheCheckCompares(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := writeFleetState(dir); err != nil {
		t.Fatalf("writeFleetState() error = %v", err)
	}
	for _, h := range fleetHosts {
		for _, name := range fleetConfigFiles {
			desired, err := assets.ReadFile("assets/config/desired/" + name)
			if err != nil {
				t.Fatalf("read desired %s: %v", name, err)
			}
			onHost, err := os.ReadFile(filepath.Join(dir, "state", h.Name, name))
			if err != nil {
				t.Errorf("%s has no %s to check: %v", h.Name, name, err)
				continue
			}
			same := bytes.Equal(desired, onHost)
			if want := !slices.Contains(h.Stale, name); same != want {
				t.Errorf("%s %s matches the desired file = %v, want %v", h.Name, name, same, want)
			}
		}
	}
}

// TestSeededInventoriesOnlyNameHostsTheDemoFills is the guard against a host that leads nowhere.
//
// The seed used to store a second inventory named staging whose one host, stage01, no run targeted
// and no fact described. It listed on the inventories page and appeared nowhere else, so following
// it reached a host page with no facts, no drift row, no fleet health, and no history, on the
// install a stranger uses to judge whether the pages in this product are connected to each other.
// Every host any seeded inventory names has to be a host the demo actually fills.
func TestSeededInventoriesOnlyNameHostsTheDemoFills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	inventories, err := stores.Inventories.List(ctx)
	if err != nil {
		t.Fatalf("Inventories.List() error = %v", err)
	}
	if len(inventories) == 0 {
		t.Fatal("seedConfig() stored no inventories")
	}
	fleet := declaredFleet(t)
	state := make(map[string]bool, len(fleetHosts))
	for _, h := range fleetHosts {
		state[h.Name] = true
	}
	filled := 0
	for _, inv := range inventories {
		for _, host := range hostsInInventory(t, inv.Content) {
			filled++
			if _, ok := fleet[host]; !ok {
				t.Errorf("inventory %s names %s, which no declared fact describes, so its host page "+
					"opens empty", inv.Name, host)
			}
			if !state[host] {
				t.Errorf("inventory %s names %s, which carries no configuration state, so it has no "+
					"drift row", inv.Name, host)
			}
		}
	}
	if filled == 0 {
		t.Error("no seeded inventory names a host, so the fleet the demo shows comes from nowhere")
	}
}

// TestWriteFleetStateReportsAnUnwritableTree pins that a state tree that cannot be written stops
// materialization rather than leaving the drift check pointed at directories that are not there.
func TestWriteFleetStateReportsAnUnwritableTree(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("in the way\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	err := writeFleetState(blocked)
	if err == nil {
		t.Fatal("writeFleetState() into a file = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "demo state for") {
		t.Errorf("writeFleetState() error = %v, want the host whose state failed named", err)
	}
}
