package main

import (
	"fmt"
	"strings"
)

// awxObjects are the names a real AWX export carries and the reads they have to be visible through
// once the migration has run.
//
// Named rather than counted. A migration that creates the right number of the wrong objects is a
// migration that moved somebody's estate into something they do not recognize, and a count agrees
// with that as readily as it agrees with a correct import.
var awxObjects = []struct {
	// Path is the listing the object should appear in.
	Path string
	// Member is the field in that listing holding the rows.
	Member string
	// Name is what the export called it, which is what an operator will look for.
	Name string
}{
	{Path: "/v1/templates", Member: "templates", Name: "Deploy Web"},
	{Path: "/v1/projects", Member: "projects", Name: "Web"},
	{Path: "/v1/inventories", Member: "inventories", Name: "Production"},
	{Path: "/v1/credentials", Member: "credentials", Name: "prod-ssh"},
	{Path: "/v1/schedules", Member: "schedules", Name: "Nightly"},
	// Every third day is a cadence cron cannot say. It comes across as the recurrence it is.
	{Path: "/v1/schedules", Member: "schedules", Name: "Every 3 days"},
}

// checkAnAWXEstateMigrates proves the deployed install turns somebody else's export into objects of
// its own that are readable by the names they had.
//
// The one-command import off AWX is the way into this product, and no deployed install had ever
// been asked to perform one. The importer is covered heavily in unit tests, which prove the
// translation and cannot prove that the shipped binary, run inside the image, against the database
// the install is actually serving, produces objects that install can then read.
//
// Two halves matter and the second is the one that is easy to skip. A migration that prints a
// summary and leaves nothing behind reads exactly like one that worked, which is why the objects
// are looked for through the API by name afterwards rather than believed from the import's own
// count.
func (h *harness) checkAnAWXEstateMigrates(phase, namespace string) {
	const claim = "an AWX export becomes objects this install serves under the names it had"
	pod, err := h.serverPod(namespace)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	export := mustManifest("awx-export.json")
	if err := h.writePodFile(namespace, pod, "/tmp/awx-export.json", export); err != nil {
		h.fail(phase, claim, err)
		return
	}

	// The assessment first, because it is what a prospect runs before they trust any of this, and
	// what it owes them is the honest half: what does not survive the move.
	assessment, err := h.kubectl("exec", "-n", namespace, pod, "--",
		"switchtender", "assess", "awx", "/tmp/awx-export.json")
	if err != nil {
		h.fail(phase, claim, fmt.Errorf("assess the export: %w\n%s", err, oneLine(assessment)))
		return
	}
	if !strings.Contains(assessment, "does not come across") {
		h.fail(phase, claim, fmt.Errorf("%w: the assessment never says what does not come across, "+
			"which is the half a reader is deciding on: %s", ErrNothingRead, oneLine(assessment)))
		return
	}

	imported, err := h.kubectl("exec", "-n", namespace, pod, "--",
		"switchtender", "import", "awx", "/tmp/awx-export.json",
		"--db", "/data/switchtender.db", "--apply")
	if err != nil {
		h.fail(phase, claim, fmt.Errorf("import the export: %w\n%s", err, oneLine(imported)))
		return
	}

	// Looked for through the API, as the objects the install now serves. This is the half that
	// separates a migration from a summary.
	var missing []string
	for _, want := range awxObjects {
		names, nerr := h.objectNames(want.Path, want.Member)
		if nerr != nil {
			h.fail(phase, claim, nerr)
			return
		}
		if !names[want.Name] {
			missing = append(missing, fmt.Sprintf("%s %q", want.Member, want.Name))
		}
	}
	if len(missing) > 0 {
		h.fail(phase, claim, fmt.Errorf("%w: the import reported success and this install does not "+
			"serve %s. %s", ErrNothingRead, strings.Join(missing, ", "), oneLine(imported)))
		return
	}
	h.pass(phase, claim, fmt.Sprintf("%d object kinds crossed and read back by name, and the "+
		"assessment said what did not: %s", len(awxObjects), oneLine(imported)))
}

// objectNames reads a listing and returns the set of names it holds, for asking whether a
// particular object is there rather than how many are.
func (h *harness) objectNames(path, member string) (map[string]bool, error) {
	var listing map[string]any
	if err := h.apiCall("GET", path, &h.human, nil, &listing); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	rows, _ := listing[member].([]any)
	names := make(map[string]bool, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				names[name] = true
			}
		}
	}
	return names, nil
}
