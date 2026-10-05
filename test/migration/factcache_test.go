package migration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireOnlyInFacts fails the scenario when value appears anywhere but the fact cache's own read
// endpoints: the chain, a receipt, a dossier, a run record, a run log, an audit listing, or a
// server log.
func (in *install) requireOnlyInFacts(what, value string) {
	in.t.Helper()
	v, ok := answers.Load(in.root)
	if !ok {
		in.t.Fatalf("no API answers were recorded")
	}
	log := v.(*answerLog)
	log.mu.Lock()
	bodies := append([]answer(nil), log.bodies...)
	log.mu.Unlock()
	for _, s := range in.servers {
		raw, err := os.ReadFile(s.logPath)
		if err != nil {
			in.t.Fatalf("read the log of server %s: %v", s.name, err)
		}
		bodies = append(bodies, answer{What: "the log of server " + s.name, Body: raw})
	}
	for _, b := range bodies {
		if strings.Contains(b.What, "/facts") {
			continue
		}
		if bytes.Contains(b.Body, []byte(value)) {
			in.t.Errorf("%s appears in %s", what, b.What)
		}
	}
}

// TestImportedFactCacheServesGatheredFactsAndKeepsThemOffTheRecord is scenario thirteen. AWX
// exports a template with use_fact_cache on. A first run gathers facts and plants a cacheable one;
// a second run that gathers nothing has to see exactly those facts from the cache; and once the
// host's cached facts are cleared, a third sees none. No fact value may reach the chain, a receipt,
// a dossier, a run record, or a log, while the fact cache setting itself is bound in every receipt
// and stated in every dossier.
func TestImportedFactCacheServesGatheredFactsAndKeepsThemOffTheRecord(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a")
	fleet := in.lookup(s, "inventories", "fleet")
	// The planted fact is read by the play from a file, so it reaches the cache without being a
	// launch input, which a run record rightly keeps.
	canary := "fact-canary-" + randomHex(t, 12)
	source := filepath.Join(in.markers, "canary-source")
	if err := os.WriteFile(source, []byte(canary), 0o600); err != nil {
		t.Fatalf("write the canary source: %v", err)
	}

	seen := func(gather bool, marker string) (string, string, string) {
		t.Helper()
		vars := map[string]any{"marker_name": marker, "gather": gather}
		rec := in.launched(s, "operator", "gather facts", map[string]any{"extra_vars": vars})
		if done := in.waitDone(s, rec.ID); done.Status != "succeeded" {
			t.Fatalf("the %s run = %s: %s", marker, done.Status, describe(done.Raw))
		}
		lines := strings.Split(strings.TrimSpace(in.marker(marker, "web1")), "\n")
		if len(lines) != 2 {
			t.Fatalf("the %s run recorded %q", marker, lines)
		}
		return rec.ID, lines[0], lines[1]
	}

	first, gatheredAt, firstCanary := seen(true, "facts-first")
	if gatheredAt == "absent" || firstCanary != canary {
		t.Fatalf("the gathering run saw time %q and canary %q, want a gathered time and %q",
			gatheredAt, firstCanary, canary)
	}
	listing := in.must(s, "operator", "GET", "/v1/inventories/"+fleet+"/facts", nil, 200)
	if !strings.Contains(string(listing.Body), "web1") {
		t.Fatalf("the fact cache holds nothing for web1 after a gathering run: %s", listing.Body)
	}

	second, cachedAt, cachedCanary := seen(false, "facts-second")
	if cachedAt != gatheredAt || cachedCanary != canary {
		t.Errorf("the run that gathered nothing saw time %q and canary %q, want the cached %q and %q",
			cachedAt, cachedCanary, gatheredAt, canary)
	}

	in.must(s, "admin", "DELETE", "/v1/inventories/"+fleet+"/facts/web1", nil, 200, 204)
	third, clearedAt, clearedCanary := seen(false, "facts-third")
	if clearedAt != "absent" || clearedCanary != "absent" {
		t.Errorf("after the cache was cleared a run saw time %q and canary %q, want neither",
			clearedAt, clearedCanary)
	}
	if dirs := in.runFileDirs(); len(dirs) != 0 {
		t.Errorf("fact cache directories left after the runs: %v", dirs)
	}

	ev := in.checkEvidence(s, first, second, third)
	for _, id := range []string{first, second, third} {
		requireRecord(t, ev.Receipts[id], recordWant{
			Launcher: "operator-laptop", OnBehalfOf: "operator", Playbook: "facts.yml",
			Hosts: []string{"web1"},
		})
		// The fact cache is part of what the run was asked to do, so the receipt binds it and the
		// dossier states it. The imported template has no timeout, which AWX defaults to.
		cache, _ := ev.Receipts[id].outcome(t).Spec["fact_cache"].(map[string]any)
		if cache == nil || cache["timeout_seconds"] != float64(0) {
			t.Errorf("the receipt of %s binds fact cache %v, want it on with no timeout", id,
				ev.Receipts[id].outcome(t).Spec["fact_cache"])
		}
		dossier := in.must(s, "admin", "GET", "/v1/runs/"+id+"/evidence", nil, 200)
		if !strings.Contains(string(dossier.Body), "uses cached facts (no timeout)") {
			t.Errorf("the dossier of %s does not state the fact cache", id)
		}
	}
	in.requireOnlyInFacts("the planted fact", canary)
	in.requireOnlyInFacts("the gathered time fact", gatheredAt)
}
